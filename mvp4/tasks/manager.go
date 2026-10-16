// Package tasks is the run's task list: IDs, dependencies, statuses and each task's worker turns
// (the "TaskManager" of DECISIONS.md § Step 2). Task state lives here, in Go, not in the director's
// context. It is a leaf capability package: it imports nothing from this module.
package tasks

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Status is the state of one task.
type Status string

const (
	Pending Status = "pending"
	Running Status = "running"
	Done    Status = "done"
	Failed  Status = "failed"
)

// Spec is one task as the director gives it to set_tasks.
type Spec struct {
	ID         string   `json:"id"`
	Task       string   `json:"task"`
	DependsOn  []string `json:"depends_on"`
	WorkerType string   `json:"worker_type"`
}

// Turn is one worker turn of a task.
type Turn struct {
	Prompt string
	Status string // the worker.Status of the turn
	Reply  string
}

// Task is one unit of work.
type Task struct {
	ID         string
	Text       string
	DependsOn  []string
	WorkerType string
	WorkerID   string // the worker that ran it ("w1")
	Status     Status
	Note       string // the task_done note, or why the task failed
	History    []Turn
}

// Manager owns the task list. Not safe for concurrent use: the run works on one task at a time.
type Manager struct {
	Tasks       []*Task
	MaxTasks    int      // the most tasks Set accepts (director.maxTasks)
	WorkerTypes []string // the known worker types; the first is the default
}

// New returns an empty manager.
func New(maxTasks int, workerTypes []string) *Manager {
	return &Manager{MaxTasks: maxTasks, WorkerTypes: workerTypes}
}

// FromTexts sets the tasks given in objective.md's ## Tasks: IDs t1, t2, …, no dependencies.
func (m *Manager) FromTexts(texts []string) error {
	specs := make([]Spec, len(texts))
	for i, t := range texts {
		specs[i] = Spec{ID: fmt.Sprintf("t%d", i+1), Task: t}
	}
	return m.Set(specs)
}

// Set replaces the task list after checking it: 1..MaxTasks tasks, non-empty unique IDs and texts, every
// dependency an existing other task, no cycles, a known worker type (empty = the default).
// [agent] Each error message says what to fix, because the director sees it and calls set_tasks again.
func (m *Manager) Set(specs []Spec) error {
	if len(specs) == 0 {
		return errors.New("tasks must contain at least one task")
	}
	if len(specs) > m.MaxTasks {
		return fmt.Errorf("%d tasks is too many; the limit is %d, so combine related steps", len(specs), m.MaxTasks)
	}
	list := make([]*Task, len(specs))
	byID := map[string]bool{}
	for i, s := range specs {
		t := &Task{ID: strings.TrimSpace(s.ID), Text: strings.TrimSpace(s.Task), WorkerType: strings.TrimSpace(s.WorkerType), Status: Pending}
		switch {
		case t.ID == "":
			return fmt.Errorf("task %d has no id", i+1)
		case byID[t.ID]:
			return fmt.Errorf("task id %q is used twice", t.ID)
		case t.Text == "":
			return fmt.Errorf("task %q has an empty task text", t.ID)
		}
		if t.WorkerType == "" && len(m.WorkerTypes) > 0 {
			t.WorkerType = m.WorkerTypes[0]
		}
		if !slices.Contains(m.WorkerTypes, t.WorkerType) {
			return fmt.Errorf("task %q: unknown worker_type %q (known: %s)", t.ID, t.WorkerType, strings.Join(m.WorkerTypes, ", "))
		}
		for _, d := range s.DependsOn {
			if d = strings.TrimSpace(d); d != "" && !slices.Contains(t.DependsOn, d) {
				t.DependsOn = append(t.DependsOn, d)
			}
		}
		byID[t.ID] = true
		list[i] = t
	}
	for _, t := range list {
		for _, d := range t.DependsOn {
			switch {
			case d == t.ID:
				return fmt.Errorf("task %q depends on itself", t.ID)
			case !byID[d]:
				return fmt.Errorf("task %q depends on %q, which is not in the list", t.ID, d)
			}
		}
	}
	if stuck := unorderable(list); len(stuck) > 0 {
		return fmt.Errorf("tasks %s depend on each other in a cycle", strings.Join(stuck, ", "))
	}
	m.Tasks = list
	return nil
}

// unorderable returns the IDs of tasks that can never become ready because their dependencies form a
// cycle: it keeps marking tasks whose dependencies are all marked, and returns what is left.
func unorderable(list []*Task) []string {
	ok := map[string]bool{}
	for progress := true; progress; {
		progress = false
		for _, t := range list {
			if !ok[t.ID] && allIn(t.DependsOn, ok) {
				ok[t.ID], progress = true, true
			}
		}
	}
	var stuck []string
	for _, t := range list {
		if !ok[t.ID] {
			stuck = append(stuck, t.ID)
		}
	}
	return stuck
}

func allIn(ids []string, set map[string]bool) bool {
	for _, id := range ids {
		if !set[id] {
			return false
		}
	}
	return true
}

// Next returns the first pending task, in list order, whose dependencies are all done; nil if none.
// [agent] With no dependencies this is plain list order, exactly the mvp3 behaviour.
func (m *Manager) Next() *Task {
	done := map[string]bool{}
	for _, t := range m.Tasks {
		done[t.ID] = t.Status == Done
	}
	for _, t := range m.Tasks {
		if t.Status == Pending && allIn(t.DependsOn, done) {
			return t
		}
	}
	return nil
}

// Get returns the task with this ID, or nil.
func (m *Manager) Get(id string) *Task {
	for _, t := range m.Tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// Started counts the tasks that are no longer pending.
func (m *Manager) Started() int {
	n := 0
	for _, t := range m.Tasks {
		if t.Status != Pending {
			n++
		}
	}
	return n
}

// Failed reports whether any task failed.
func (m *Manager) Failed() bool {
	return slices.ContainsFunc(m.Tasks, func(t *Task) bool { return t.Status == Failed })
}

// NotDone returns the tasks that are not done, as "id (status)".
func (m *Manager) NotDone() []string {
	var open []string
	for _, t := range m.Tasks {
		if t.Status != Done {
			open = append(open, fmt.Sprintf("%s (%s)", t.ID, t.Status))
		}
	}
	return open
}
