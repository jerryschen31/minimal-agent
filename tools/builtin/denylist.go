package builtin

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// deniedNames are file or directory names (matched case-insensitively against every path component)
// that the file tools refuse even inside the allowed directory, because they usually hold credentials.
// ".agent-trace" is the default trace directory name, refused anywhere (e.g. another project's traces copied in);
// the configured traceDir, whatever its name, is refused by path (see NewReadFileTool).
// ".env.example" is the one exception to the ".env.*" pattern.
var deniedNames = []string{".env", ".env.*", "*.pem", "*.key", "id_rsa*", ".ssh", ".agent-trace"}

// DeniedNames returns a copy of the built-in deny list, for display (-mode info). ".env.example" is still allowed.
func DeniedNames() []string { return slices.Clone(deniedNames) }

// DenyList is the set of name patterns the file tools never read: the built-in deniedNames plus any the config adds
// (denyFileNames). Config can extend the list but never shrink it, so the zero value already refuses the built-ins.
// [agent] It's a "default" by the three-question test (DECISIONS.md § Policy out of the core): it guards against the
// model asking for a secret, so a missing or empty config key must not turn it off.
type DenyList struct {
	extra []string // the config's patterns, lowercased; matched after the built-ins
}

// NewDenyList adds extra patterns (path.Match syntax, e.g. "*.secret" or "credentials.json") to the built-in list.
// A pattern is matched against each single path component, so it must not be empty or contain "/"; a malformed
// pattern is an error, because a typo here would otherwise silently protect nothing.
func NewDenyList(extra []string) (DenyList, error) {
	var d DenyList
	for _, p := range extra {
		if strings.TrimSpace(p) == "" || strings.ContainsAny(p, `/\`) {
			return DenyList{}, fmt.Errorf("denyFileNames: %q must be a non-empty file or directory name pattern, without / or \\", p)
		}
		if _, err := path.Match(p, ""); err != nil {
			return DenyList{}, fmt.Errorf("denyFileNames: %q: %w", p, err)
		}
		d.extra = append(d.extra, strings.ToLower(p))
	}
	return d, nil
}

// Patterns returns every pattern in force, built-ins first.
func (d DenyList) Patterns() []string { return slices.Concat(deniedNames, d.extra) }

// IsDenied reports whether any component of rel (a slash- or OS-separated relative path) matches the list.
func (d DenyList) IsDenied(rel string) bool {
	patterns := d.Patterns()
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		name := strings.ToLower(part) // case-insensitive filesystems (macOS, Windows) would otherwise let ".ENV" through
		if name == ".env.example" {
			continue
		}
		for _, pattern := range patterns {
			if ok, _ := path.Match(pattern, name); ok {
				return true
			}
		}
	}
	return false
}
