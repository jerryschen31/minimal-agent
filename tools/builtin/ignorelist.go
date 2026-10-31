package builtin

import (
	"fmt"
	"path"
	"strings"
)

// IgnoreList is the set of name patterns (config ignorePaths) that the walking file tools (list_files, later
// search_files) leave out: build output, caches, dependencies and git's own data, which are noise to the model and
// would use up the listing caps. A pattern ending in "/" matches directories only (".git/"), otherwise it matches
// files and directories ("*.pyc"). An ignored directory is shown but not walked, so the model knows it exists and can
// list it directly. The zero value ignores nothing.
// [agent] Unlike DenyList, this is user policy, not a protection (DECISIONS.md § Policy out of the core): config
// replaces the default list, and [] turns it off. Listing an ignored directory directly still works.
type IgnoreList struct {
	patterns []ignorePattern
}

type ignorePattern struct {
	name    string // lowercased path.Match pattern for one name, without the trailing "/"
	dirOnly bool   // the pattern ended in "/"
}

// NewIgnoreList checks patterns (path.Match syntax on one name, optionally ending in "/"). A pattern that is empty,
// has a "/" anywhere but the end, has a "\", or is malformed is an error: it would otherwise silently ignore nothing.
func NewIgnoreList(patterns []string) (IgnoreList, error) {
	var l IgnoreList
	for _, p := range patterns {
		name, dirOnly := strings.CutSuffix(p, "/")
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, `/\`) {
			return IgnoreList{}, fmt.Errorf("ignorePaths: %q must be a non-empty file or directory name pattern (a trailing / means directories only), without other / or \\", p)
		}
		if _, err := path.Match(name, ""); err != nil {
			return IgnoreList{}, fmt.Errorf("ignorePaths: %q: %w", p, err)
		}
		l.patterns = append(l.patterns, ignorePattern{name: strings.ToLower(name), dirOnly: dirOnly})
	}
	return l, nil
}

// Ignores reports whether an entry called name (one path component) is on the list. The match ignores case, like the
// deny list.
func (l IgnoreList) Ignores(name string, isDir bool) bool {
	name = strings.ToLower(name)
	for _, p := range l.patterns {
		if p.dirOnly && !isDir {
			continue
		}
		if ok, _ := path.Match(p.name, name); ok {
			return true
		}
	}
	return false
}

// String lists the patterns as configured (lowercased), for the tool description; "" when the list is empty.
func (l IgnoreList) String() string {
	shown := make([]string, len(l.patterns))
	for i, p := range l.patterns {
		shown[i] = p.name
		if p.dirOnly {
			shown[i] += "/"
		}
	}
	return strings.Join(shown, ", ")
}
