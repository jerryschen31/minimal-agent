package builtin

import (
	"slices"
	"strings"
)

// deniedNames are the built-in deny patterns (pathPattern rules; all are plain names, so they match at any depth)
// for files and directories that usually hold credentials. The file tools refuse them even inside the allowed
// directory. ".agent-trace" is the default trace directory name, refused anywhere (e.g. another project's traces
// copied in); the configured traceDir, whatever its name, is refused by path (see NewWorkspace).
// ".env.example" is the one exception, to ".env.*" and to every other pattern.
var deniedNames = []string{".env", ".env.*", "*.pem", "*.key", "id_rsa*", ".ssh", ".agent-trace"}

// builtinDenied is deniedNames parsed once; they are known-good, so a parse error is a bug.
var builtinDenied = func() []pathPattern {
	p, err := parsePathPatterns("deniedNames", deniedNames)
	if err != nil {
		panic(err)
	}
	return p
}()

// DeniedNames returns a copy of the built-in deny list, for display (-mode info).
//
// Notes:
//   - ".env.example" is still allowed.
func DeniedNames() []string { return slices.Clone(deniedNames) }

// DenyList is the set of path patterns the file tools never touch: the built-in deniedNames plus any the config adds
// (denyPaths). Config can extend the list but never shrink it, so the zero value already refuses the built-ins.
// [agent] It's a "default" by the three-question test (DECISIONS.md § Policy out of the core): it guards against the
// model asking for a secret, so a missing or empty config key must not turn it off.
type DenyList struct {
	extra []pathPattern // the config's patterns; matched after the built-ins
}

// NewDenyList adds extra patterns (pathPattern rules, e.g. "*.secret", "secrets/", "config/creds.json") to the
// built-in list.
//
// Notes:
//   - A malformed pattern is an error, because a typo here would otherwise silently protect nothing.
func NewDenyList(extra []string) (DenyList, error) {
	p, err := parsePathPatterns("denyPaths", extra)
	if err != nil {
		return DenyList{}, err
	}
	return DenyList{extra: p}, nil
}

// Patterns returns every pattern in force, built-ins first.
//
// Notes: none.
func (d DenyList) Patterns() []string { return slices.Concat(deniedNames, rawPatterns(d.extra)) }

// IsDenied reports whether rel (relative to workDir, slash- or OS-separated) or any directory above it matches the
// list.
//
// Notes:
//   - isDir says whether rel itself is a directory (for patterns ending in "/").
//   - ".env.example" as the last name is never matched (isEnvExample).
func (d DenyList) IsDenied(rel string, isDir bool) bool {
	return matchPath(slices.Concat(builtinDenied, d.extra), rel, isDir, isEnvExample)
}

// isEnvExample is the one name the deny list never matches: a template that holds no real secrets.
//
// Notes:
//   - The comparison ignores case.
func isEnvExample(name string) bool { return strings.EqualFold(name, ".env.example") }
