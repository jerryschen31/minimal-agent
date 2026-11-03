package textutils

import "unicode/utf8"

// CutRunes returns s cut to at most n characters (runes), and whether anything was cut.
//
// Notes:
//   - A string of n characters or fewer is returned as it is.
//   - A negative n is treated as 0.
//   - Invalid UTF-8 bytes each count as one character and are kept, not replaced.
func CutRunes(s string, n int) (string, bool) {
	n = max(n, 0)
	count := 0
	for i := range s { // i is the byte offset of each character
		if count == n {
			return s[:i], true
		}
		count++
	}
	return s, false
}

// CutBytes returns s cut to at most limit bytes, backing up so a multi-byte character is never split, and whether
// anything was cut.
//
// Notes:
//   - A string of limit bytes or fewer is returned as it is.
//   - A negative limit is treated as 0.
//   - Only a cut that lands inside a valid multi-byte character moves, and then to that character's first byte. Bytes
//     that were already invalid UTF-8 are kept as they are: a stray continuation byte after the cut ("é\x80x" cut at
//     2) is not part of the é before it, so the é stays.
//   - The search for the character's first byte goes back at most utf8.UTFMax-1 bytes, so a run of stray continuation
//     bytes does not eat the result.
func CutBytes(s string, limit int) (string, bool) {
	limit = max(limit, 0)
	if len(s) <= limit {
		return s, false
	}
	cut := limit
	// s[limit] is the first byte left out; a continuation byte there may mean a character began earlier
	if !utf8.RuneStart(s[limit]) {
		for start := limit - 1; start >= 0 && start >= limit-(utf8.UTFMax-1); start-- {
			if !utf8.RuneStart(s[start]) {
				continue
			}
			// decode the whole character from its first byte: back up only if it is a valid multi-byte character that
			// really crosses the cut (an invalid sequence decodes as one byte and is kept)
			if _, size := utf8.DecodeRuneInString(s[start:]); size > 1 && start+size > limit {
				cut = start
			}
			break
		}
	}
	return s[:cut], true
}

// TrimPartialRune drops an incomplete multi-byte character from the end of b.
//
// Notes:
//   - Looks back at most utf8.UTFMax bytes; a complete final character, or no start byte in that range, leaves b
//     unchanged.
func TrimPartialRune(b []byte) []byte {
	for i := 1; i <= utf8.UTFMax && i <= len(b); i++ {
		if utf8.RuneStart(b[len(b)-i]) {
			if !utf8.FullRune(b[len(b)-i:]) {
				return b[:len(b)-i]
			}
			return b
		}
	}
	return b
}
