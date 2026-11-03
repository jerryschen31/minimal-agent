package trace

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jerryschen31/minimal-agent/textutils"
)

const (
	shortenAbove  = 1000 // tool arguments up to this many bytes are recorded as they are
	bodyKeptChars = 200  // characters of a file body kept in the trace; the rest is replaced by a byte count
	cutMarkerFmt  = "… [%d bytes in all, cut for the trace]"
)

// bodyKeys are the argument names that hold file text: what write_file and edit_file send, and what an MCP server's
// write tool usually calls its text.
var bodyKeys = []string{"content", "old_string", "new_string"}

// shortenArguments returns a tool call's arguments for the trace, with long file bodies cut.
//
// Notes:
//   - [agent] A write_file or edit_file call carries whole file text (up to 1 MB) that the trace does not need: the
//     path, the sizes and the result say what happened. Cutting it also keeps secrets pasted into a file out of the
//     log. The conversation itself (memory) still holds the full call; only the trace is shortened.
//   - Arguments up to shortenAbove bytes are recorded as they are.
//   - A JSON object: each string value whose key is in bodyKeys (compared case-insensitively, because the tools decode
//     keys that way, so "Content" is a body too) and is longer than bodyKeptChars is cut. Other fields, such as a long
//     shell command the digest shows, are untouched. After a cut the keys come back in sorted order.
//   - Anything else that long (invalid JSON, which sloppy models emit and which may still hold a whole file; or valid
//     JSON that isn't an object) is cut as a whole to shortenAbove characters, so nothing oversized reaches the trace
//     in a shape this function can't read.
func shortenArguments(args string) string {
	if len(args) <= shortenAbove {
		return args
	}
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil || m == nil { // invalid JSON, or valid but not an object (null decodes to a nil map)
		return cutChars(args, shortenAbove)
	}
	cut := false
	for k, v := range m {
		s, ok := v.(string)
		if !ok || !isBodyKey(k) || len([]rune(s)) <= bodyKeptChars {
			continue
		}
		m[k] = cutChars(s, bodyKeptChars)
		cut = true
	}
	if !cut {
		return args
	}
	b, err := json.Marshal(m)
	if err != nil {
		return cutChars(args, shortenAbove)
	}
	return string(b)
}

// isBodyKey reports whether an argument name holds file text, ignoring case.
//
// Notes: none.
func isBodyKey(k string) bool {
	for _, b := range bodyKeys {
		if strings.EqualFold(k, b) {
			return true
		}
	}
	return false
}

// cutChars returns s cut to n characters followed by a marker with its size in bytes.
//
// Notes:
//   - A string of n characters or fewer is returned as it is.
func cutChars(s string, n int) string {
	cut, wasCut := textutils.CutRunes(s, n)
	if !wasCut {
		return s
	}
	return cut + fmt.Sprintf(cutMarkerFmt, len(s))
}
