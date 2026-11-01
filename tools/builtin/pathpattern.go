package builtin

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// pathPattern is one .gitignore-style pattern from config denyPaths or ignorePaths (or a built-in deny name). Paths
// are always relative to workDir. The rules, as in .gitignore:
//
//   - no "/" (".env", "*.pyc", "secrets"): matches a file or directory with that name at any depth;
//   - a "/" at the start or in the middle ("/build", "secrets/*.env"): anchored, matched against the whole path from
//     workDir ("*" never crosses a "/", so "secrets/*.env" matches secrets/a.env, not secrets/old/a.env or
//     app/secrets/a.env);
//   - a trailing "/" ("node_modules/"): directories only (still at any depth unless there is another "/").
//
// A matched directory covers everything under it, so the way to cover a whole tree is to name the directory. Matching
// ignores case, because macOS and Windows filesystems do. Not supported: "!" negation, and "**", which is refused.
type pathPattern struct {
	raw      string // as configured, lowercased; shown in errors and tool descriptions
	glob     string // path.Match pattern, lowercased, without the leading or trailing "/"
	anchored bool   // matched against the whole relative path, not only the last name
	dirOnly  bool   // matches directories only
}

// parsePathPatterns checks and parses raw patterns; key ("denyPaths", "ignorePaths") starts each error. A pattern that
// would silently match nothing is an error: empty, spaces at either end, "\", an empty, "." or ".." part, or malformed.
func parsePathPatterns(key string, raw []string) ([]pathPattern, error) {
	var out []pathPattern
	for _, r := range raw {
		p, err := parsePathPattern(r)
		if err != nil {
			return nil, fmt.Errorf("%s: %q: %w", key, r, err)
		}
		out = append(out, p)
	}
	return out, nil
}

func parsePathPattern(r string) (pathPattern, error) {
	if r != strings.TrimSpace(r) {
		return pathPattern{}, fmt.Errorf("has spaces at the start or end")
	}
	if strings.Contains(r, `\`) {
		return pathPattern{}, fmt.Errorf(`must use / between directories, not \`)
	}
	p := pathPattern{raw: strings.ToLower(r)}
	glob := p.raw
	glob, p.dirOnly = strings.CutSuffix(glob, "/")
	glob, p.anchored = strings.CutPrefix(glob, "/")
	if strings.Contains(glob, "/") {
		p.anchored = true
	}
	if strings.Contains(glob, "**") {
		// [agent] path.Match would read "**" as two "*", which never cross "/": "**/*.secret" would look recursive but
		// miss a/b/x.secret. Refused so a rule never protects less than it appears to.
		return pathPattern{}, fmt.Errorf(`"**" is not supported; to cover a whole tree, name the directory (e.g. "secrets/")`)
	}
	for _, part := range strings.Split(glob, "/") {
		if part == "" || part == "." || part == ".." {
			return pathPattern{}, fmt.Errorf("must name a file or directory, without empty, . or .. parts")
		}
	}
	if _, err := path.Match(glob, ""); err != nil {
		return pathPattern{}, err
	}
	p.glob = glob
	return p, nil
}

// match reports whether this one pattern matches rel itself (lowercased, "/"-separated, relative to workDir).
func (p pathPattern) match(rel string, isDir bool) bool {
	if p.dirOnly && !isDir {
		return false
	}
	target := rel
	if !p.anchored {
		target = path.Base(rel)
	}
	ok, _ := path.Match(p.glob, target)
	return ok
}

// matchPath reports whether rel or any directory above it matches one of patterns, so a matched directory covers
// everything under it. rel is relative to workDir, with "/" or the OS separator; isDir is about rel itself (every
// part above it is a directory). skip, if not nil, exempts the LAST name from matching (DenyList's ".env.example");
// a directory with that name further up is matched as usual, so it can't carry denied files out from under a pattern.
func matchPath(patterns []pathPattern, rel string, isDir bool, skip func(name string) bool) bool {
	rel = strings.ToLower(filepath.ToSlash(rel))
	if rel == "." || rel == "" {
		return false // workDir itself is never matched
	}
	parts := strings.Split(rel, "/")
	for i := range parts {
		if skip != nil && i == len(parts)-1 && skip(parts[i]) {
			continue
		}
		prefix := strings.Join(parts[:i+1], "/")
		dir := isDir || i < len(parts)-1
		for _, p := range patterns {
			if p.match(prefix, dir) {
				return true
			}
		}
	}
	return false
}

// rawPatterns returns the patterns as configured, for display.
func rawPatterns(patterns []pathPattern) []string {
	out := make([]string, len(patterns))
	for i, p := range patterns {
		out[i] = p.raw
	}
	return out
}
