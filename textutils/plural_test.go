package textutils

import "testing"

// - Verify Plural picks the singular only for exactly 1
func Test_Unit_Plural(t *testing.T) {
	for n, want := range map[int]string{0: "calls", 1: "call", 2: "calls"} {
		if got := Plural(n, "call", "calls"); got != want {
			t.Errorf("Plural(%d) = %q, want %q", n, got, want)
		}
	}
}
