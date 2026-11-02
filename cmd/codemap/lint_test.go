package main

import (
	"strings"
	"testing"
)

const goodHeader = "Foo does it.\n\nNotes:\n  - a detail\n"

func Test_HeaderProblems(t *testing.T) {
	tests := []struct {
		name    string
		f       Func
		wantSub []string // each must appear in some problem; nil means no problems
	}{
		{"complete header is accepted", Func{Name: "Foo", Header: goodHeader}, nil},
		{"Notes: none. on one line is accepted", Func{Name: "Foo", Header: "Foo does it.\n\nNotes: none.\n"}, nil},
		{"Notes with free text on the same line is accepted", Func{Name: "Foo", Header: "Foo does it.\n\nNotes: edge case X.\n"}, nil},
		{"Example is not required", Func{Name: "Foo", Header: "Foo does it.\n\nNotes: none.\n"}, nil},
		{"Example is allowed", Func{Name: "Foo", Header: "Foo does it.\n\nExample:\n\n\tFoo()\n\nNotes: none.\n"}, nil},
		{"empty header", Func{Name: "Foo"}, []string{"no header"}},
		{"whitespace-only header", Func{Name: "Foo", Header: " \n\n"}, []string{"no header"}},
		{"wrong first word", Func{Name: "Foo", Header: "Bar does it.\n\nNotes: none.\n"}, []string{`must start with "Foo"`}},
		{"first word is the bare method name", Func{Name: "Ptr", Key: "Box.Ptr", Header: "Ptr does it.\n\nNotes: none.\n"}, nil},
		{"missing Notes", Func{Name: "Foo", Header: "Foo does it.\n"}, []string{`"Notes:"`}},
		{"Example without Notes still needs Notes", Func{Name: "Foo", Header: "Foo does it.\n\nExample:\n\n\tFoo()\n"}, []string{`"Notes:"`}},
		{"Notes: with nothing after it is a heading", Func{Name: "Foo", Header: "Foo does it.\n\nNotes:\n  - x\n"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := headerProblems(tc.f)
			if tc.wantSub == nil {
				if len(got) != 0 {
					t.Fatalf("unexpected problems: %v", got)
				}
				return
			}
			for _, sub := range tc.wantSub {
				if !contains(got, sub) {
					t.Errorf("no problem containing %q in %v", sub, got)
				}
			}
		})
	}
}

func Test_Lint_Rules(t *testing.T) {
	ok := Func{Key: "Foo", Name: "Foo", Header: goodHeader}
	clean := Pkg{Dir: "p", PkgDocFiles: []string{"doc.go"}, Funcs: []Func{ok}}

	if got := lint([]Pkg{clean}); len(got) != 0 {
		t.Fatalf("clean package reported %v", got)
	}
	tests := []struct {
		name string
		mut  func(p *Pkg)
		want string
	}{
		{"package comment outside doc.go", func(p *Pkg) { p.PkgDocFiles = []string{"doc.go", "x.go"} }, "package comment must be only in doc.go"},
		{"package comment only in another file", func(p *Pkg) { p.PkgDocFiles = []string{"x.go"} }, "package comment must be only in doc.go"},
		{"no package comment at all", func(p *Pkg) { p.PkgDocFiles = nil }, "package comment must be only in doc.go"},
		{"missing header", func(p *Pkg) { p.Funcs[0].Header = "" }, "p: Foo: no header"},
		{"wrong first word", func(p *Pkg) { p.Funcs[0].Header = strings.Replace(goodHeader, "Foo", "Baz", 1) }, "header must start with"},
		{"missing Notes", func(p *Pkg) { p.Funcs[0].Header = "Foo does it.\n" }, `no "Notes:"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := clean
			p.Funcs = append([]Func(nil), clean.Funcs...)
			p.PkgDocFiles = append([]string(nil), clean.PkgDocFiles...)
			tc.mut(&p)
			if got := lint([]Pkg{p}); !contains(got, tc.want) {
				t.Errorf("want a problem containing %q, got %v", tc.want, got)
			}
		})
	}
}

func Test_Lint_ReportsEveryFunctionOfEveryPackage(t *testing.T) {
	bad := Func{Key: "A", Name: "A"}
	got := lint([]Pkg{
		{Dir: "one", PkgDocFiles: []string{"doc.go"}, Funcs: []Func{bad}},
		{Dir: "two", PkgDocFiles: []string{"doc.go"}, Funcs: []Func{bad}},
	})
	if !contains(got, "one: A: no header") || !contains(got, "two: A: no header") {
		t.Errorf("got %v", got)
	}
}

func Test_Lint_FixtureReportsHeaderlessAndAcceptsDocumented(t *testing.T) {
	got := lint(mustLoad(t, copyFixture(t)))
	if !contains(got, ".: Undocumented: no header") {
		t.Errorf("headerless function not reported: %v", got)
	}
	if contains(got, "Documented: ") && !contains(got, "Undocumented: ") {
		t.Errorf("unexpected: %v", got)
	}
	for _, m := range got {
		if strings.HasPrefix(m, ".: Documented:") {
			t.Errorf("well-formed header reported: %s", m)
		}
		if strings.Contains(m, "unreviewed") || strings.Contains(m, "body changed") {
			t.Errorf("removed rule still reported: %s", m)
		}
	}
}
