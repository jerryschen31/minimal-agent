package builtin

import "strings"

// IgnoreList is the set of path patterns (config ignorePaths, pathPattern rules) that the walking file tools
// (list_files, later search_files) treat as off limits: build output, caches, dependencies and git's own data, which
// are noise to the model and would use up the listing caps. A matched directory covers everything under it: it is
// shown as "dir/ (ignored)" but not walked, and listing it directly is refused. The zero value ignores nothing.
// [agent] Unlike DenyList, this is user policy, not a protection (DECISIONS.md § Policy out of the core): config
// replaces the default list, and [] turns it off.
type IgnoreList struct {
	patterns []pathPattern
}

// NewIgnoreList checks and parses the ignore patterns.
//
// Notes:
//   - A malformed pattern is an error, since it would otherwise silently ignore nothing.
func NewIgnoreList(patterns []string) (IgnoreList, error) {
	p, err := parsePathPatterns("ignorePaths", patterns)
	if err != nil {
		return IgnoreList{}, err
	}
	return IgnoreList{patterns: p}, nil
}

// Ignores reports whether rel (relative to workDir) or any directory above it is on the list.
//
// Notes:
//   - isDir says whether rel itself is a directory (for patterns ending in "/").
func (l IgnoreList) Ignores(rel string, isDir bool) bool {
	return matchPath(l.patterns, rel, isDir, nil)
}

// String lists the patterns as configured (lowercased), for the tool description; "" when the list is empty.
//
// Notes: none.
func (l IgnoreList) String() string { return strings.Join(rawPatterns(l.patterns), ", ") }
