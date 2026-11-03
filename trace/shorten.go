package trace

import (
	"encoding/json"
	"fmt"
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
//   - Only string values under bodyKeys longer than bodyKeptChars are cut, and only when the arguments are longer than
//     shortenAbove, so other tools' arguments (a long shell command the digest shows) are untouched.
//   - Arguments that are not a JSON object are returned as they are. After a cut the keys come back in sorted order.
func shortenArguments(args string) string {
	if len(args) <= shortenAbove {
		return args
	}
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return args
	}
	cut := false
	for _, k := range bodyKeys {
		if s, ok := m[k].(string); ok && len([]rune(s)) > bodyKeptChars {
			m[k] = string([]rune(s)[:bodyKeptChars]) + fmt.Sprintf(cutMarkerFmt, len(s))
			cut = true
		}
	}
	if !cut {
		return args
	}
	b, err := json.Marshal(m)
	if err != nil {
		return args
	}
	return string(b)
}
