package tasks

import (
	"strings"
	"testing"
)

func newTestManager() *Manager { return New(5, []string{"general"}) }

// - Verify Set's checks: each bad list is rejected with a message naming the problem, and leaves the
// previous list in place
func Test_Manager_Set_Rejects(t *testing.T) {
	m := newTestManager()
	if err := m.FromTexts([]string{"keep me"}, false); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		specs []Spec
		want  string
	}{
		{"empty", nil, "at least one"},
		{"too many", make([]Spec, 6), "limit is 5"},
		{"no id", []Spec{{Task: "x"}}, "no id"},
		{"duplicate id", []Spec{{ID: "a", Task: "x"}, {ID: "a", Task: "y"}}, "used twice"},
		{"empty text", []Spec{{ID: "a", Task: " "}}, "empty task text"},
		{"unknown worker type", []Spec{{ID: "a", Task: "x", WorkerType: "gpu"}}, `unknown worker_type "gpu"`},
		{"unknown dependency", []Spec{{ID: "a", Task: "x", DependsOn: []string{"b"}}}, `depends on "b"`},
		{"self dependency", []Spec{{ID: "a", Task: "x", DependsOn: []string{"a"}}}, "depends on itself"},
		{"cycle", []Spec{{ID: "a", Task: "x"}, {ID: "b", Task: "y", DependsOn: []string{"c"}}, {ID: "c", Task: "z", DependsOn: []string{"b"}}}, "tasks b, c depend on each other"},
	} {
		if err := m.Set(c.specs); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", c.name, err, c.want)
		}
	}
	if len(m.Tasks) != 1 || m.Tasks[0].Text != "keep me" {
		t.Errorf("a rejected Set changed the list: %+v", m.Tasks)
	}
}

// - Verify Next follows list order but waits for dependencies, and returns nil once nothing is ready
func Test_Manager_Next_DependencyOrder(t *testing.T) {
	m := newTestManager()
	err := m.Set([]Spec{
		{ID: "api", Task: "write the API", DependsOn: []string{"model"}},
		{ID: "model", Task: "write the data model"},
		{ID: "docs", Task: "write the README", DependsOn: []string{"api", "model", "api"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := m.Get("docs"); len(d.DependsOn) != 2 || d.WorkerType != "general" {
		t.Errorf("docs = %+v, want duplicates dropped and the default worker type", d)
	}
	var order []string
	for t := m.Next(); t != nil; t = m.Next() {
		order = append(order, t.ID)
		t.Status = Done
	}
	if got := strings.Join(order, ","); got != "model,api,docs" {
		t.Errorf("order = %s, want model,api,docs", got)
	}
	if m.Started() != 3 || m.Failed() || len(m.NotDone()) != 0 {
		t.Errorf("Started=%d Failed=%v NotDone=%v", m.Started(), m.Failed(), m.NotDone())
	}
}

// - Verify FromTexts numbers the given tasks t1, t2, … with no dependencies, or chained with chain set
func Test_Manager_FromTexts(t *testing.T) {
	m := newTestManager()
	if err := m.FromTexts([]string{"a", "b"}, false); err != nil {
		t.Fatal(err)
	}
	if m.Tasks[0].ID != "t1" || m.Tasks[1].ID != "t2" || m.Next().ID != "t1" || m.Tasks[1].DependsOn != nil {
		t.Errorf("tasks = %+v %+v", m.Tasks[0], m.Tasks[1])
	}
	if err := m.FromTexts([]string{"a", "b", "c"}, true); err != nil {
		t.Fatal(err)
	}
	if d := m.Tasks[2].DependsOn; m.Tasks[0].DependsOn != nil || len(d) != 1 || d[0] != "t2" {
		t.Errorf("chained tasks = %+v %+v", m.Tasks[0], m.Tasks[2])
	}
}
