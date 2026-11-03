//go:build !unix

package builtin

import "os"

// linkCount returns how many names point at the file fi describes, and false if the system does not say.
//
// Notes:
//   - Always false on this platform: the hard-link check in openExisting is skipped here (Go's FileInfo has no link
//     count outside Unix).
func linkCount(fi os.FileInfo) (uint64, bool) { return 0, false }
