package textutils

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// - Verify CutRunes counts characters, not bytes, and says whether it cut.
func Test_Unit_CutRunes(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		n       int
		want    string
		wantCut bool
	}{
		{"shorter", "héllo", 10, "héllo", false},
		{"exactly n", "héllo", 5, "héllo", false},
		{"cut ascii", "hello", 2, "he", true},
		{"cut keeps whole multibyte chars", "héllo wörld", 2, "hé", true},
		{"emoji is one character", "😀😀😀", 2, "😀😀", true},
		{"zero", "abc", 0, "", true},
		{"negative is zero", "abc", -3, "", true},
		{"empty", "", 3, "", false},
		{"empty with zero", "", 0, "", false},
		{"invalid byte counts as one", "a\xffb", 2, "a\xff", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, cut := CutRunes(tt.in, tt.n)
			if got != tt.want || cut != tt.wantCut {
				t.Errorf("CutRunes(%q, %d) = %q, %v; want %q, %v", tt.in, tt.n, got, cut, tt.want, tt.wantCut)
			}
		})
	}
}

// - Verify CutBytes never splits a character: for every limit the result is valid UTF-8, within the limit, a prefix of
// the input, and no shorter than needed (at most UTFMax-1 bytes lost).
func Test_Unit_CutBytes_NeverSplitsACharacter(t *testing.T) {
	in := "aé€😀z" // 1, 2, 3 and 4 byte characters
	for limit := 0; limit <= len(in)+1; limit++ {
		got, cut := CutBytes(in, limit)
		if !utf8.ValidString(got) {
			t.Errorf("limit %d: %q is not valid UTF-8", limit, got)
		}
		if len(got) > limit || !strings.HasPrefix(in, got) {
			t.Errorf("limit %d: %q is not a prefix within the limit", limit, got)
		}
		if limit-len(got) > utf8.UTFMax-1 && cut {
			t.Errorf("limit %d: backed up too far, kept %d bytes", limit, len(got))
		}
		if wantCut := len(in) > limit; cut != wantCut {
			t.Errorf("limit %d: cut = %v, want %v", limit, cut, wantCut)
		}
	}
}

// - Verify the exact results at the interesting limits, and that short input is returned as it is.
func Test_Unit_CutBytes_Results(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		limit   int
		want    string
		wantCut bool
	}{
		{"fits", "héllo", 10, "héllo", false},
		{"exactly limit", "héllo", 6, "héllo", false},
		{"ascii cut", "hello", 3, "hel", true},
		{"cut inside é backs up", "héllo", 2, "h", true},
		{"cut after é keeps it", "héllo", 3, "hé", true},
		{"cut inside emoji", "a😀", 3, "a", true},
		{"stray continuation byte after a complete character", "é\x80x", 2, "é", true},
		{"stray continuation byte after a character, cut one earlier", "é\x80x", 1, "", true},
		{"stray continuation bytes after ascii", "a\x80\x80\x80\x80", 3, "a\x80\x80", true},
		{"valid character split by a stray byte is still whole", "aé\x80", 2, "a", true},
		{"negative is zero", "abc", -1, "", true},
		{"empty", "", 0, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, cut := CutBytes(tt.in, tt.limit)
			if got != tt.want || cut != tt.wantCut {
				t.Errorf("CutBytes(%q, %d) = %q, %v; want %q, %v", tt.in, tt.limit, got, cut, tt.want, tt.wantCut)
			}
		})
	}
}

// - Verify bytes that were already invalid UTF-8 are kept, and a run of stray continuation bytes does not make the
// cut back up past UTFMax-1 bytes (the old ToValidUTF8 approach would have dropped them).
func Test_Unit_CutBytes_InvalidInputIsKept(t *testing.T) {
	in := "ab" + strings.Repeat("\x80", 10) // continuation bytes with no start byte
	got, cut := CutBytes(in, 8)
	if got != in[:8] || !cut {
		t.Errorf("CutBytes = %q, %v; want the first 8 bytes unchanged", got, cut)
	}
	got, _ = CutBytes("a\xffb\xffc", 4)
	if got != "a\xffb\xff" {
		t.Errorf("CutBytes = %q; want invalid bytes kept", got)
	}
}

// - Verify TrimPartialRune drops only an incomplete final character.
func Test_Unit_TrimPartialRune(t *testing.T) {
	euro := []byte("€") // 3 bytes
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"empty", nil, ""},
		{"ascii", []byte("abc"), "abc"},
		{"complete multibyte", append([]byte("a"), euro...), "a€"},
		{"one byte of three", append([]byte("a"), euro[:1]...), "a"},
		{"two bytes of three", append([]byte("a"), euro[:2]...), "a"},
		{"only a partial", euro[:2], ""},
		{"no start byte in range", []byte{0x80, 0x80, 0x80, 0x80, 0x80}, "\x80\x80\x80\x80\x80"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(TrimPartialRune(tt.in)); got != tt.want {
				t.Errorf("TrimPartialRune(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
