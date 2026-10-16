package director

import (
	"errors"
	"fmt"
	"strings"
)

// TaskStatus is the state of one task.
type TaskStatus string

const (
	TaskPending TaskStatus = "pending"
	TaskDone    TaskStatus = "done"
	TaskFailed  TaskStatus = "failed"
)

// Task is one unit of work from the task list.
type Task struct {
	Text   string
	Status TaskStatus
	Note   string // the task_done note, or the reason the task failed
}

// Phase is where the run is. The code moves the run between phases; the director never picks one.
type Phase string

const (
	PhasePlan   Phase = "plan"   // no "## Tasks" in objective.md: the director must call set_tasks
	PhaseWork   Phase = "work"   // tasks are worked through in order
	PhaseReview Phase = "review" // all tasks done, or one failed: the director must call finish
	PhaseDone   Phase = "done"   // finish was accepted
)

// Plan is the run's task list and progress. The control tools change it; the run loop reads it.
// [agent] It is the "code owns the plan" half of the design (DECISIONS.md § Step 1: task-driven flow):
// every transition is a method with a phase check, so a tool called at the wrong time is an error
// observation for the director, never a corrupted plan.
// Not safe for concurrent use: tool calls within a turn run one after another.
type Plan struct {
	Phase   Phase
	Tasks   []Task
	Current int // index of the current task during PhaseWork

	MaxTasks int // the most tasks set_tasks accepts (director.maxTasks in the config)

	// WorkerTurnsLeft is how many worker turns the current task has left. The run loop sets it before each
	// director turn in PhaseWork; task_failed uses it to refuse a give-up while the worker could still retry.
	WorkerTurnsLeft int
	failRefused     bool // task_failed was already refused once for the current task

	// set by Finish
	Success bool
	Summary string
	Gaps    []string
}

// NewPlan starts a plan from the objective's tasks; with none, the run starts in PhasePlan.
// maxTasks limits what set_tasks will accept.
func NewPlan(tasks []string, maxTasks int) *Plan {
	p := &Plan{Phase: PhasePlan, MaxTasks: maxTasks}
	if len(tasks) > 0 {
		p.setTasks(tasks)
	}
	return p
}

func (p *Plan) setTasks(tasks []string) {
	p.Tasks = make([]Task, len(tasks))
	for i, t := range tasks {
		p.Tasks[i] = Task{Text: t, Status: TaskPending}
	}
	p.Current = 0
	p.Phase = PhaseWork
}

// SetTasks records the director's own task list (PLAN phase only).
func (p *Plan) SetTasks(tasks []string) error {
	if p.Phase != PhasePlan {
		return fmt.Errorf("set_tasks is only allowed while planning; the run is in the %s phase", p.Phase)
	}
	var clean []string
	for _, t := range tasks {
		if t = strings.TrimSpace(t); t != "" {
			clean = append(clean, t)
		}
	}
	if len(clean) == 0 {
		return errors.New("tasks must contain at least one non-empty task")
	}
	if len(clean) > p.MaxTasks {
		return fmt.Errorf("%d tasks is too many; the limit is %d, so combine related steps", len(clean), p.MaxTasks)
	}
	p.setTasks(clean)
	return nil
}

// CurrentTask returns the current task's index and text; ok is false outside PhaseWork.
func (p *Plan) CurrentTask() (i int, text string, ok bool) {
	if p.Phase != PhaseWork || p.Current >= len(p.Tasks) {
		return 0, "", false
	}
	return p.Current, p.Tasks[p.Current].Text, true
}

// TaskDone marks the current task done and moves to the next one, or to REVIEW after the last.
func (p *Plan) TaskDone(note string) error {
	if p.Phase != PhaseWork {
		return fmt.Errorf("task_done is only allowed while working on tasks; the run is in the %s phase", p.Phase)
	}
	p.Tasks[p.Current].Status = TaskDone
	p.Tasks[p.Current].Note = strings.TrimSpace(note)
	p.Current++
	p.failRefused = false
	if p.Current >= len(p.Tasks) {
		p.Phase = PhaseReview
	}
	return nil
}

// TaskFailed marks the current task failed and moves the run to REVIEW (a failed task stops the work).
func (p *Plan) TaskFailed(reason string) error {
	if p.Phase != PhaseWork {
		return fmt.Errorf("task_failed is only allowed while working on tasks; the run is in the %s phase", p.Phase)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("reason must not be empty")
	}
	p.Tasks[p.Current].Status = TaskFailed
	p.Tasks[p.Current].Note = reason
	p.Phase = PhaseReview
	return nil
}

// Failed reports whether any task failed.
func (p *Plan) Failed() bool {
	for _, t := range p.Tasks {
		if t.Status == TaskFailed {
			return true
		}
	}
	return false
}

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
	if success {
		var open []string
		for i, t := range p.Tasks {
			if t.Status != TaskDone {
				open = append(open, fmt.Sprintf("%d (%s)", i+1, t.Status))
			}
		}
		if len(open) > 0 {
			return fmt.Errorf("success=true is not allowed: tasks %s are not done; call finish with success=false and list what is missing as gaps", strings.Join(open, ", "))
		}
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
