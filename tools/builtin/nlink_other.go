//go:build !unix

package builtin

import "os"

// linkCount returns how many names point at the file fi describes, and false if the system does not say.
//
// Notes:
//   - Always false on this platform: the hard-link checks (openExisting, copy_file, move_file) are skipped here, because
//     Go's FileInfo has no link count outside Unix. Windows has hard links too, so there a write, edit, copy or move
//     through another name of .env or .git/config is NOT stopped. Accepted for now; the real fix (read the count from
//     an open file with GetFileInformationByHandle) is in DECISIONS.md § PR #38 review.
func linkCount(fi os.FileInfo) (uint64, bool) { return 0, false }
