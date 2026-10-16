package director

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jerryschen31/minimal-agent/mvp4/tasks"
	"github.com/jerryschen31/minimal-agent/mvp4/worker"
)

// Phase is where the run is. The code moves the run between phases; the director never picks one.
type Phase string

const (
	PhasePlan   Phase = "plan"   // no "## Tasks" in objective.md: the director must call set_tasks
	PhaseWork   Phase = "work"   // tasks are worked through one at a time, in dependency order
	PhaseReview Phase = "review" // all tasks done, or one failed: the director must call finish
	PhaseDone   Phase = "done"   // finish was accepted
)

// Plan is the run's phase, current task and review. The task list itself is in Tasks (a tasks.Manager,
// the TaskManager of DECISIONS.md § Step 2). The control tools change it; the run loop reads it.
// [agent] It is the "code owns the plan" half of the design (DECISIONS.md § Step 1: task-driven flow):
// every transition is a method with a phase check, so a tool called at the wrong time is an error
// observation for the director, never a corrupted plan.
// Not safe for concurrent use: tool calls within a turn run one after another.
type Plan struct {
	Phase   Phase
	Tasks   *tasks.Manager
	Current *tasks.Task // the running task during PhaseWork; nil between tasks

	// WorkerTurnsLeft is how many worker turns the current task has left. The run loop sets it before each
	// director turn in PhaseWork; task_failed uses it to refuse a give-up while the worker could still retry.
	WorkerTurnsLeft int
	failRefused     bool // task_failed was already refused once for the current task

	// set by Finish
	Success bool
	Summary string
	Gaps    []string
}

// NewPlan starts a plan from the objective's tasks (IDs t1, t2, …); with none, the run starts in
// PhasePlan. maxTasks limits what set_tasks will accept.
func NewPlan(given []string, maxTasks int) *Plan {
	p := &Plan{Phase: PhasePlan, Tasks: tasks.New(maxTasks, []string{worker.DefaultType})}
	// ParseObjective has already checked the count and that no task is empty, so this can't fail
	if len(given) > 0 && p.Tasks.FromTexts(given) == nil {
		p.Phase = PhaseWork
	}
	return p
}

// SetTasks records the director's own task list (PLAN phase only).
func (p *Plan) SetTasks(specs []tasks.Spec) error {
	if p.Phase != PhasePlan {
		return fmt.Errorf("set_tasks is only allowed while planning; the run is in the %s phase", p.Phase)
	}
	if err := p.Tasks.Set(specs); err != nil {
		return err
	}
	p.Phase = PhaseWork
	return nil
}

// StartNext marks the next ready task running and makes it the current task (PhaseWork, between tasks).
func (p *Plan) StartNext() (*tasks.Task, error) {
	if p.Phase != PhaseWork || p.Current != nil {
		return nil, fmt.Errorf("a task can only start between tasks in the work phase; the run is in the %s phase", p.Phase)
	}
	t := p.Tasks.Next()
	if t == nil {
		return nil, errors.New("no task is ready to start") // Set rejects cycles, so this is a bug
	}
	t.Status = tasks.Running
	p.Current, p.failRefused = t, false
	return t, nil
}

// TaskDone marks the current task done. The work goes on while tasks are pending; after the last one,
// the run moves to REVIEW.
func (p *Plan) TaskDone(note string) error {
	if p.Phase != PhaseWork || p.Current == nil {
		return fmt.Errorf("task_done is only allowed while working on a task; the run is in the %s phase", p.Phase)
	}
	p.Current.Status = tasks.Done
	p.Current.Note = strings.TrimSpace(note)
	p.Current = nil
	if p.Tasks.Next() == nil {
		p.Phase = PhaseReview
	}
	return nil
}

// TaskFailed marks the current task failed and moves the run to REVIEW (a failed task stops the work).
func (p *Plan) TaskFailed(reason string) error {
	if p.Phase != PhaseWork || p.Current == nil {
		return fmt.Errorf("task_failed is only allowed while working on a task; the run is in the %s phase", p.Phase)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("reason must not be empty")
	}
	p.Current.Status = tasks.Failed
	p.Current.Note = reason
	p.Current = nil
	p.Phase = PhaseReview
	return nil
}

// Failed reports whether any task failed.
func (p *Plan) Failed() bool { return p.Tasks.Failed() }

// Finish records the review (REVIEW phase only). success=true is refused while any task is not done:
// that would contradict the task list, so the director gets the error and can answer again.
func (p *Plan) Finish(success bool, summary string, gaps []string) error {
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
