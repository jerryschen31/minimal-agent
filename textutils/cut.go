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
//   - Only a cut that lands inside a character moves. Bytes that were already invalid UTF-8 are kept as they are, and
//     the cut backs up at most utf8.UTFMax-1 bytes, so a run of stray continuation bytes does not eat the result.
func CutBytes(s string, limit int) (string, bool) {
	limit = max(limit, 0)
	if len(s) <= limit {
		return s, false
	}
	cut := limit
	// s[cut] is the first byte left out; a continuation byte there means the character began earlier
	for back := 0; back < utf8.UTFMax-1 && cut > 0 && !utf8.RuneStart(s[cut]); back++ {
		cut--
	}
	if !utf8.RuneStart(s[cut]) { // no start byte nearby: these bytes are not a character, cut where asked
		cut = limit
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
