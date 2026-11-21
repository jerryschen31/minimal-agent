package textutils

// Plural returns one when n is 1, otherwise many: Plural(n, "call", "calls").
//
// Notes:
//   - Moved here from trace when agent needed it too (PR #46 review).
func Plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
