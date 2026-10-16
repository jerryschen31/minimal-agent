package director

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jerryschen31/minimal-agent/mvp4/tasks"
	"github.com/jerryschen31/minimal-agent/mvp4/worker"
)

// Phase is where the run is. The code moves the run between phases; the director never picks one.
type Phase string

const (
	PhasePlan   Phase = "plan"   // no "## Tasks" in objective.md: the director must call set_tasks
	PhaseWork   Phase = "work"   // tasks are worked on in dependency order, up to Parallel at a time
	PhaseReview Phase = "review" // all tasks done, or one failed and the rest have ended: the director must call finish
	PhaseDone   Phase = "done"   // finish was accepted
)

// Plan is the run's phase and review. The task list itself is in Tasks (a tasks.Manager, the TaskManager
// of DECISIONS.md § Step 2). The control tools change it; the run loop reads it.
// [agent] It is the "code owns the plan" half of the design (DECISIONS.md § Step 1: task-driven flow):
// every transition is a method with a phase check, so a tool called at the wrong time is an error
// observation for the director, never a corrupted plan.
//
// With parallel tasks (orchestrator mode, director.maxParallel > 1) several task goroutines change the
// plan at once, so it has one lock for itself and its tasks (DECISIONS.md § Slice D, item 2 of the open
// decisions). Its methods take the lock themselves; other code that reads or changes task state while
// tasks run takes it with Lock/Unlock. It is never held during an LLM or worker call.
type Plan struct {
	mu       sync.Mutex
	Phase    Phase
	Tasks    *tasks.Manager
	Parallel int // how many tasks may run at once (director.maxParallel); shown to the director

	// set by Finish
	Success bool
	Summary string
	Gaps    []string
}

// TaskRun is one running task as its director conversation sees it: what task_done and task_failed act
// on. Each running task has its own, used by one goroutine only, so its fields need no lock.
type TaskRun struct {
	Task *tasks.Task

	// WorkerTurnsLeft is how many worker turns the task has left. The run loop sets it before each director
	// turn; task_failed uses it to refuse a give-up while the worker could still retry.
	WorkerTurnsLeft int
	failRefused     bool // task_failed was already refused once for this task
}

// NewPlan starts a plan from the objective's tasks (IDs t1, t2, …); with none, the run starts in
// PhasePlan. maxTasks limits what set_tasks will accept; parallel is how many tasks may run at once;
// workerTypes are the known worker types, the first being the default (nil = just worker.DefaultType).
// [agent] Given tasks are chained (each depends on the one before) when parallel > 1: a hand-written
// ## Tasks list reads as a sequence, and running it all at once would be surprising and racy. With one
// task at a time the chain changes nothing, so it is left out (and the task list looks as before).
func NewPlan(given []string, maxTasks, parallel int, workerTypes []string) *Plan {
	if len(workerTypes) == 0 {
		workerTypes = []string{worker.DefaultType}
	}
	p := &Plan{Phase: PhasePlan, Tasks: tasks.New(maxTasks, workerTypes), Parallel: max(parallel, 1)}
	// ParseObjective has already checked the count and that no task is empty, so this can't fail
	if len(given) > 0 && p.Tasks.FromTexts(given, p.Parallel > 1) == nil {
		p.Phase = PhaseWork
	}
	return p
}

// Lock and Unlock guard the plan and its tasks; see Plan.
func (p *Plan) Lock()   { p.mu.Lock() }
func (p *Plan) Unlock() { p.mu.Unlock() }

// SetTasks records the director's own task list (PLAN phase only).
func (p *Plan) SetTasks(specs []tasks.Spec) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Phase != PhasePlan {
		return fmt.Errorf("set_tasks is only allowed while planning; the run is in the %s phase", p.Phase)
	}
	if err := p.Tasks.Set(specs); err != nil {
		return err
	}
	p.Phase = PhaseWork
	return nil
}

// StartNext marks the next ready task whose worker type has a free worker (free reports that) running,
// and returns it. It returns nil when no task can start now: none is ready (the rest wait for running
// tasks), no worker of a ready task's type is free, a task has failed (the round drains: running tasks
// finish, no new one starts), or the work phase is over.
func (p *Plan) StartNext(free func(workerType string) bool) *TaskRun {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Phase != PhaseWork || p.Tasks.Failed() {
		return nil
	}
	t := p.Tasks.NextFor(func(t *tasks.Task) bool { return free(t.WorkerType) })
	if t == nil {
		return nil
	}
	t.Status = tasks.Running
	return &TaskRun{Task: t}
}

// TaskDone marks a running task done.
func (p *Plan) TaskDone(run *TaskRun, note string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.checkRunning(run, "task_done"); err != nil {
		return err
	}
	run.Task.Status = tasks.Done
	run.Task.Note = strings.TrimSpace(note)
	p.maybeEndWork()
	return nil
}

// TaskFailed marks a running task failed. No new task starts after that (StartNext); the round moves
// to REVIEW once the tasks still running have ended.
func (p *Plan) TaskFailed(run *TaskRun, reason string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.checkRunning(run, "task_failed"); err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("reason must not be empty")
	}
	run.Task.Status = tasks.Failed
	run.Task.Note = reason
	p.maybeEndWork()
	return nil
}

// checkRunning checks that a task tool may act on run now. Call with p.mu held.
func (p *Plan) checkRunning(run *TaskRun, tool string) error {
	if p.Phase != PhaseWork || run == nil || run.Task.Status != tasks.Running {
		return fmt.Errorf("%s is only allowed while working on a task; the run is in the %s phase", tool, p.Phase)
	}
	return nil
}

// maybeEndWork moves the run to REVIEW once no task is running and none will start: all are done, or
// one failed (the drain is over). Call with p.mu held.
func (p *Plan) maybeEndWork() {
	for _, t := range p.Tasks.Tasks {
		if t.Status == tasks.Running {
			return
		}
	}
	if p.Tasks.Failed() || p.Tasks.Next() == nil {
		p.Phase = PhaseReview
	}
}

// Failed reports whether any task failed.
func (p *Plan) Failed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Tasks.Failed()
}

// Finish records the review (REVIEW phase only). success=true is refused while any task is not done:
// that would contradict the task list, so the director gets the error and can answer again.
func (p *Plan) Finish(success bool, summary string, gaps []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Phase != PhaseReview {
		return fmt.Errorf("finish is only allowed in the review phase; the run is in the %s phase", p.Phase)
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return errors.New("summary must not be empty")
	}
	if open := p.Tasks.NotDone(); success && len(open) > 0 {
		return fmt.Errorf("success=true is not allowed: tasks %s are not done; call finish with success=false and list what is missing as gaps", strings.Join(open, ", "))
	}
	p.Success, p.Summary = success, summary
	p.Gaps = nil
	for _, g := range gaps {
		if g = strings.TrimSpace(g); g != "" {
			p.Gaps = append(p.Gaps, g)
		}
	}
	p.Phase = PhaseDone
	return nil
}
