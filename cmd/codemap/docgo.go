package main

import (
	"errors"
	"go/format"
	"go/parser"
	"go/token"
	"strings"
)

// heading starts the generated part of a package comment. It is the last section of the comment, so the section
// runs from this line to the end of the comment and needs no end marker.
const heading = "# Codemap (auto-generated)"

// replaceSection returns doc.go source with the generated section replaced by lines (each already prefixed with //).
//
// Notes:
//   - With no section yet, the section is appended to the end of the package comment, after a blank comment line.
//   - The section is always the last part of the comment, so it runs to the end of the comment and needs no end marker.
//   - The result is passed through gofmt (go/format), so the output is stable when run again.
//   - A /* */ package comment or a file with no package comment is an error.
func replaceSection(src []byte, lines []string) ([]byte, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "doc.go", src, parser.ParseComments|parser.PackageClauseOnly)
	if err != nil {
		return nil, err
	}
	if f.Doc == nil {
		return nil, errors.New("no package comment to add the codemap to")
	}
	section := strings.Join(lines, "\n")
	end := fset.Position(f.Doc.End()).Offset
	start, prefix := end, "\n//\n"
	for _, c := range f.Doc.List {
		if !strings.HasPrefix(c.Text, "//") {
			return nil, errors.New("package comment must use // comments")
		}
		if strings.TrimSpace(strings.TrimPrefix(c.Text, "//")) == heading {
			start, prefix = fset.Position(c.Pos()).Offset, ""
			break
		}
	}
	out := append(append(append([]byte{}, src[:start]...), prefix+section...), src[end:]...)
	return format.Source(out)
}

// extractSection returns the generated section of doc.go as plain text lines, starting after the heading.
//
// Notes:
//   - Comment markers are removed but tabs are kept, which is how codeBlock finds the preformatted part.
//   - ok is false when there is no package comment or no heading.
func extractSection(src []byte) (lines []string, ok bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "doc.go", src, parser.ParseComments|parser.PackageClauseOnly)
	if err != nil || f.Doc == nil {
		return nil, false
	}
	all := strings.Split(f.Doc.Text(), "\n")
	for i, l := range all {
		if l == heading {
			return all[i+1:], true
		}
	}
	return nil, false
}

// codeBlock returns the preformatted part of a section (the tab-indented lines), with one tab removed from each.
//
// Notes:
//   - Everything before the first tab-indented line (the intro text) is dropped; trailing blank lines are trimmed.
func codeBlock(lines []string) []string {
	first := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "\t") {
			first = i
			break
		}
	}
	if first < 0 {
		return nil
	}
	var out []string
	for _, l := range lines[first:] {
		out = append(out, strings.TrimPrefix(l, "\t"))
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return out
}
