package plan

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/objective"
)

// [agent] the Cleanup tests (and mustCleanup, fileOf, checkFixes, checkTasks) are in cleanup_test.go.

// complete is a hand-built plan that passes CheckComplete and uses every field.
func complete() Plan {
	return Plan{
		Format:        FormatV1,
		ObjectivePath: "objective.md",
		ObjectiveHash: "sha256:abc",
		Version:       3,
		Title:         "Build a Fibonacci CLI",
		Summary:       "Module, code, tests.",
		Assumptions:   "- Go 1.26 is installed.",
		TaskNotes:     "Work through these in order.",
		Tasks: []Task{
			{ID: "t1", Title: "Create the module", Needs: "a Go toolchain", DoneWhen: "go.mod exists",
				Body: "Run go mod init example.com/fib."},
			{ID: "t2", Title: "Write fib", Depends: []string{"t1"}, Reason: "the objective now asks for fib(n)",
				Body: "Write fib.go with fib(n int) []int."},
			{ID: "t3", Title: "Publish", Depends: []string{"t1", "t2"}, Effects: EffectsExternal,
				Gate: "ask before running (publishes)", Needs: "the package registry, logged in",
				Body: "Steps:\n\n- tag it\n- push it\n\n```sh\ngit tag v0.1.0\n```"},
		},
		Other: []Section{{"Notes", "Keep the README short."}},
	}
}

// completeMarkdown is complete().Markdown(), written out by hand.
var completeMarkdown = strings.Join([]string{
	"---",
	"format: minagent-plan/v1",
	"objective: objective.md",
	"objective_hash: sha256:abc",
	"plan_version: 3",
	"---",
	"",
	"# Plan: Build a Fibonacci CLI",
	"Module, code, tests.",
	"",
	"## Assumptions",
	"- Go 1.26 is installed.",
	"",
	"## Notes",
	"Keep the README short.",
	"",
	"## Tasks",
	"Work through these in order.",
	"",
	"### t1: Create the module",
	"needs: a Go toolchain",
	"done when: go.mod exists",
	"",
	"Run go mod init example.com/fib.",
	"",
	"### t2: Write fib",
	"depends: t1",
	"reason: the objective now asks for fib(n)",
	"",
	"Write fib.go with fib(n int) []int.",
	"",
	"### t3: Publish",
	"depends: t1, t2",
	"effects: external",
	"gate: ask before running (publishes)",
	"needs: the package registry, logged in",
	"",
	"Steps:",
	"",
	"- tag it",
	"- push it",
	"",
	"```sh",
	"git tag v0.1.0",
	"```",
}, "\n") + "\n"

// checkRoundTrip fails the test unless Cleanup(p.Markdown()) makes no fix and gives back p.
func checkRoundTrip(t *testing.T, p Plan) {
	t.Helper()
	md := p.Markdown()
	p2, fixes, err := Cleanup([]byte(md))
	if err != nil {
		t.Fatalf("Cleanup(Markdown()): %v\nmarkdown:\n%s", err, md)
	}
	if len(fixes) != 0 {
		t.Errorf("Cleanup(Markdown()) made fixes: %v\nmarkdown:\n%s", fixes, md)
	}
	if !reflect.DeepEqual(p2, p) {
		t.Errorf("round trip changed the plan:\n got  %#v\n want %#v\nmarkdown:\n%s", p2, p, md)
	}
}

// --- Task IDs ---

// - Verify AssignTaskIDs gives missing IDs the next free number, never renumbers, and never goes below nextID (so a
// removed task's ID is not reused)
func Test_Unit_AssignTaskIDs(t *testing.T) {
	tests := []struct {
		name        string
		ids         []string // "" = no ID
		nextID      int
		want        []string
		wantChanged []int
	}{
		{"none have IDs", []string{"", "", ""}, 0, []string{"t1", "t2", "t3"}, []int{0, 1, 2}},
		{"all have IDs, out of order", []string{"t1", "t5", "t2"}, 0, []string{"t1", "t5", "t2"}, nil},
		{"mixed", []string{"t1", "", "t5", ""}, 0, []string{"t1", "t6", "t5", "t7"}, []int{1, 3}},
		{"non-numeric ID is kept and ignored", []string{"setup", ""}, 0, []string{"setup", "t1"}, []int{1}},
		{"too long to be a number", []string{"t1234567890", ""}, 0, []string{"t1234567890", "t1"}, []int{1}},
		{"empty", nil, 0, nil, nil},
		{"nextID above the highest", []string{"t1", ""}, 8, []string{"t1", "t8"}, []int{1}},
		{"nextID below the highest", []string{"t9", ""}, 3, []string{"t9", "t10"}, []int{1}},
		{"nextID 1 is the same as none", []string{""}, 1, []string{"t1"}, []int{0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tasks []Task
			for _, id := range tt.ids {
				tasks = append(tasks, Task{ID: id})
			}
			changed := AssignTaskIDs(tasks, tt.nextID)
			var got []string
			for _, task := range tasks {
				got = append(got, task.ID)
			}
			if !slices.Equal(got, tt.want) || !slices.Equal(changed, tt.wantChanged) {
				t.Errorf("IDs %v, changed %v; want %v, %v", got, changed, tt.want, tt.wantChanged)
			}
		})
	}
}

// --- Markdown ---

// - Verify Cleanup(p.Markdown()) gives back p with zero fixes, for every testdata file and hand-built plans
func Test_Unit_Markdown_RoundTrip(t *testing.T) {
	files, err := filepath.Glob("testdata/*.md")
	if err != nil || len(files) == 0 {
		t.Fatalf("no testdata files (err %v)", err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			p, _ := mustCleanup(t, string(data))
			checkRoundTrip(t, p)
		})
	}
	t.Run("complete", func(t *testing.T) { checkRoundTrip(t, complete()) })
	t.Run("empty Other section", func(t *testing.T) {
		p := complete()
		p.Other = []Section{{Heading: "Notes"}}
		checkRoundTrip(t, p)
	})
	t.Run("task notes only", func(t *testing.T) {
		checkRoundTrip(t, Plan{Format: FormatV1, Title: "x", TaskNotes: "No tasks yet."})
	})
	t.Run("no title", func(t *testing.T) {
		checkRoundTrip(t, Plan{Format: FormatV1, Tasks: []Task{{ID: "t1", Title: "a"}}})
	})
}

// - Verify Markdown's layout: front matter first, sections in order, field lines in standard order, empty optional
// parts left out, one "\n" at the end
func Test_Unit_Markdown_Layout(t *testing.T) {
	if got := complete().Markdown(); got != completeMarkdown {
		t.Errorf("Markdown():\n%s\nwant:\n%s", got, completeMarkdown)
	}

	p := Plan{Title: "x", Tasks: []Task{{ID: "t1", Title: "Only"}}}
	want := "---\nformat: minagent-plan/v1\n---\n\n# Plan: x\n\n## Tasks\n\n### t1: Only\n"
	if got := p.Markdown(); got != want {
		t.Errorf("minimal Markdown():\n%q\nwant:\n%q", got, want)
	}

	if got, want := (Plan{}).Markdown(), "---\nformat: minagent-plan/v1\n---\n\n# Plan\n"; got != want {
		t.Errorf("empty Markdown() = %q; want %q", got, want)
	}
}

// --- CheckComplete ---

// - Verify a complete plan passes, each problem is named, and several problems give one joined error
func Test_Unit_CheckComplete(t *testing.T) {
	const maxTasks = 4
	tests := []struct {
		name     string
		edit     func(p *Plan) // changes complete() to break it; nil = leave it complete
		wantSubs []string      // substrings the error must contain; empty = want nil
	}{
		{"complete", nil, nil},
		{"empty format is v1", func(p *Plan) { p.Format = "" }, nil},
		{"no title is fine", func(p *Plan) { p.Title = "" }, nil},
		{"unknown format", func(p *Plan) { p.Format = "minagent-plan/v9" }, []string{`format "minagent-plan/v9" is not supported`}},
		{"no objective path", func(p *Plan) { p.ObjectivePath = " " }, []string{`"objective:" is missing`}},
		{"no objective hash", func(p *Plan) { p.ObjectiveHash = "" }, []string{`"objective_hash:" is missing`}},
		{"no version", func(p *Plan) { p.Version = 0 }, []string{`"plan_version:" is 0`}},
		{"no tasks", func(p *Plan) { p.Tasks = nil }, []string{`"## Tasks" has no tasks`}},
		{"too many tasks", func(p *Plan) {
			p.Tasks = append(p.Tasks, Task{ID: "t4", Title: "a"}, Task{ID: "t5", Title: "b"})
		}, []string{"there are 5 tasks; the limit is 4"}},
		{"missing ID", func(p *Plan) { p.Tasks[0].ID = "" },
			[]string{`task 1 ("Create the module"): ID "" is not t<number>`}},
		{"non-numeric ID", func(p *Plan) { p.Tasks[0].ID = "setup" }, []string{`ID "setup" is not t<number>`}},
		{"duplicate ID", func(p *Plan) { p.Tasks[2].ID = "t1"; p.Tasks[2].Depends = nil },
			[]string{"task ID t1 is used by more than one task"}},
		{"blank title", func(p *Plan) { p.Tasks[1].Title = "  " }, []string{"task t2 has no title"}},
		{"bad effects", func(p *Plan) { p.Tasks[0].Effects = "local" }, []string{`task t1: effects "local" is not "external"`}},
		{"depends on a missing task", func(p *Plan) { p.Tasks[1].Depends = []string{"t9"} },
			[]string{`task t2 depends on "t9", which is not a task in this file`}},
		{"depends on itself", func(p *Plan) { p.Tasks[1].Depends = []string{"t2"} },
			[]string{"task t2 depends on itself"}},
		{"cycle", func(p *Plan) { p.Tasks[0].Depends = []string{"t2"} },
			[]string{"in a loop (t1 → t2 → t1)"}},
		{"two problems at once", func(p *Plan) { p.ObjectiveHash = ""; p.Tasks = nil },
			[]string{`"objective_hash:" is missing`, `"## Tasks" has no tasks`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := complete()
			if tt.edit != nil {
				tt.edit(&p)
			}
			err := p.CheckComplete(maxTasks)
			if len(tt.wantSubs) == 0 {
				if err != nil {
					t.Errorf("CheckComplete = %v; want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckComplete = nil; want an error containing %q", tt.wantSubs)
			}
			for _, sub := range tt.wantSubs {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("CheckComplete = %v; want it to contain %q", err, sub)
				}
			}
		})
	}
}

// - Verify depending on itself is reported once, not also as a loop
func Test_Unit_CheckComplete_SelfDependencyIsNotACycle(t *testing.T) {
	p := complete()
	p.Tasks[0].Depends = []string{"t1"}
	err := p.CheckComplete(4)
	if err == nil || strings.Contains(err.Error(), "loop") {
		t.Errorf("CheckComplete = %v; want only the self-dependency", err)
	}
}

// - Verify findCycle: no cycle, a two-task cycle, a longer one reached from an earlier task, and missing tasks
func Test_Unit_FindCycle(t *testing.T) {
	task := func(id string, deps ...string) Task { return Task{ID: id, Depends: deps} }
	tests := []struct {
		name  string
		tasks []Task
		want  []string
	}{
		{"none", []Task{task("t1"), task("t2", "t1"), task("t3", "t1", "t2")}, nil},
		{"two tasks", []Task{task("t1", "t2"), task("t2", "t1")}, []string{"t1", "t2", "t1"}},
		{"three tasks, entered from outside", []Task{task("t1", "t2"), task("t2", "t3"), task("t3", "t4"),
			task("t4", "t2")}, []string{"t2", "t3", "t4", "t2"}},
		{"missing and self dependencies are skipped", []Task{task("t1", "t9", "t1"), task("t2", "t1")}, nil},
	}
	for _, tt := range tests {
		if got := findCycle(tt.tasks); !slices.Equal(got, tt.want) {
			t.Errorf("%s: findCycle = %v; want %v", tt.name, got, tt.want)
		}
	}
}

// - Verify Fix.String with and without a line number, and for Was "" (added) and Now "" (ignored)
func Test_Unit_Fix_String(t *testing.T) {
	tests := []struct {
		fix  Fix
		want string
	}{
		{Fix{7, "### Tasks", "## Tasks"}, `line 7: "### Tasks" → "## Tasks"`},
		{Fix{2, "", "```"}, "line 2: added \"```\""},
		{Fix{3, "name: fib", ""}, `line 3: ignored "name: fib"`},
		{Fix{0, "a", "b"}, `"a" → "b"`},
	}
	for _, tt := range tests {
		if got := tt.fix.String(); got != tt.want {
			t.Errorf("%#v.String() = %q; want %q", tt.fix, got, tt.want)
		}
	}
}

// --- Hashes and staleness ---

// - Verify DefinitionHash ignores the reason line and cosmetic differences, and changes with any other field, the
// title or the body
func Test_Unit_DefinitionHash(t *testing.T) {
	base := complete().Tasks[2]
	h := base.DefinitionHash()
	if !strings.HasPrefix(h, "sha256:") || len(h) != len("sha256:")+64 {
		t.Fatalf("DefinitionHash = %q; want sha256:<64 hex digits>", h)
	}
	reasoned := base
	reasoned.Reason = "a new reason"
	if reasoned.DefinitionHash() != h {
		t.Error("a new reason changed the hash")
	}
	for name, edit := range map[string]func(t *Task){
		"id":        func(t *Task) { t.ID = "t9" },
		"title":     func(t *Task) { t.Title += "!" },
		"depends":   func(t *Task) { t.Depends = []string{"t1"} },
		"effects":   func(t *Task) { t.Effects = "" },
		"gate":      func(t *Task) { t.Gate = "" },
		"needs":     func(t *Task) { t.Needs = "a browser" },
		"done when": func(t *Task) { t.DoneWhen = "it's done" },
		"body":      func(t *Task) { t.Body = "changed" },
	} {
		changed := base
		edit(&changed)
		if changed.DefinitionHash() == h {
			t.Errorf("%s: the hash did not change", name)
		}
	}

	tidy, _ := mustCleanup(t, fileOf("## Tasks", "### t1: a", "depends: t2", "", "- x"))
	messy, fixes := mustCleanup(t, fileOf("## Tasks", "### T1 - a", "Depends:T2", "", "-   x"))
	if len(fixes) == 0 || tidy.Tasks[0].DefinitionHash() != messy.Tasks[0].DefinitionHash() {
		t.Errorf("a cosmetic difference changed the hash (fixes %v)", fixes)
	}
}

// - Verify IsStale: same hash → fresh; a different hash or no hash in the plan → stale
func Test_Unit_IsStale(t *testing.T) {
	p := complete()
	if p.IsStale("sha256:abc") {
		t.Error("same hash: stale")
	}
	if !p.IsStale("sha256:def") {
		t.Error("different hash: not stale")
	}
	p.ObjectiveHash = ""
	if !p.IsStale("") {
		t.Error("no hash in the plan: not stale")
	}
}

// - Verify External reports "effects: external" only
func Test_Unit_Task_External(t *testing.T) {
	if !(Task{Effects: EffectsExternal}).External() || (Task{}).External() || (Task{Effects: "local"}).External() {
		t.Error("External() is wrong")
	}
}

// --- Fuzzing ---

// FuzzCleanup: for any input, Cleanup never panics, and when it succeeds, Cleanup(p.Markdown()) succeeds with zero
// fixes and gives back p. Run: go test ./plan -run XXX -fuzz FuzzCleanup -fuzztime 30s
func FuzzCleanup(f *testing.F) {
	files, _ := filepath.Glob("testdata/*.md")
	for _, path := range files {
		if data, err := os.ReadFile(path); err == nil {
			f.Add(data)
		}
	}
	for _, s := range []string{
		completeMarkdown,
		"",
		"# Plan: x\n",
		"## Tasks\n- a\n  more\n- [x] b\n### t5: c\ndepends on: task1, 3\nNeeds: x\nREASON : y\n",
		"## Assumptions\n```\nunclosed\n## Tasks\n### t1: x\n",
		"## Tasks\n### t1: x\nneeds: a\nneeds: b\n\ngate: c\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		p, fixes, err := Cleanup(data)
		if err != nil {
			return // rejecting an input is fine; panicking is not
		}
		for _, x := range fixes {
			if x.Was == x.Now {
				t.Fatalf("a fix that changes nothing: %#v", x)
			}
		}
		checkRoundTrip(t, p)
	})
}

// --- Documentation ---

// - Verify examples/objective-example.plan.md is in the standard form (zero fixes), ready to run, exactly what
// Markdown writes, and not stale against the example objective it names
func Test_Unit_ExampleFile(t *testing.T) {
	data, err := os.ReadFile("../examples/objective-example.plan.md")
	if err != nil {
		t.Fatal(err)
	}
	p, fixes := mustCleanup(t, string(data))
	checkFixes(t, fixes, nil)
	if err := p.CheckComplete(20); err != nil {
		t.Errorf("CheckComplete: %v", err)
	}
	if got := p.Markdown(); got != string(data) {
		t.Errorf("the example is not exactly what Markdown writes:\n%s", got)
	}
	objData, err := os.ReadFile(filepath.Join("../examples", p.ObjectivePath))
	if err != nil {
		t.Fatal(err)
	}
	o, err := objective.Parse(objData)
	if err != nil {
		t.Fatal(err)
	}
	if p.IsStale(o.Hash()) {
		t.Errorf("the example plan is stale: objective_hash %s, the example objective hashes to %s", p.ObjectiveHash, o.Hash())
	}
}
