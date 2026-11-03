//go:build unix

package builtin

import (
	"os"
	"syscall"
)

// linkCount returns how many directory entries (names) point at the file fi describes, and false if the system does
// not say.
//
// Notes:
//   - Reads st_nlink from the stat the OS returned.
func linkCount(fi os.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Nlink), true
}
