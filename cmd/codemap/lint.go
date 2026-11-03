package main

import (
	"fmt"
	"slices"
	"strings"
)

// lint reports everything the header rules forbid; an empty result means nothing is wrong.
//
// Notes:
//   - It checks shape only (a header exists, starts with the function's name, has Notes); whether the words are still
//     true after a body change is for the doc-keeper to judge when it reads the diff.
func lint(pkgs []Pkg) []string {
	var out []string
	for _, p := range pkgs {
		if !slices.Equal(p.PkgDocFiles, []string{"doc.go"}) {
			out = append(out, fmt.Sprintf("%s: package comment must be only in doc.go (found in %v)", p.Dir, p.PkgDocFiles))
		}
		for _, f := range p.Funcs {
			where := fmt.Sprintf("%s: %s", p.Dir, f.Key)
			for _, m := range headerProblems(f) {
				out = append(out, where+": "+m)
			}
		}
	}
	return out
}

// headerProblems checks the shape of one function's header.
//
// Notes:
//   - The first word must be the bare function name, even for a method.
//   - "Notes: none." on a single line counts as a Notes section.
func headerProblems(f Func) []string {
	if strings.TrimSpace(f.Header) == "" {
		return []string{"no header"}
	}
	var out []string
	if first := strings.Fields(f.Header)[0]; first != f.Name {
		out = append(out, fmt.Sprintf("header must start with %q", f.Name))
	}
	var hasNotes bool
	for _, l := range strings.Split(f.Header, "\n") {
		hasNotes = hasNotes || l == "Notes:" || strings.HasPrefix(l, "Notes: ")
	}
	if !hasNotes {
		out = append(out, `header has no "Notes:" line`)
	}
	return out
}
