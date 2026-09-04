//go:build windows

package project

import "os"

// fileOwnerUID is unimplementable on Windows (NTFS uses SIDs, not POSIX UIDs)
// and __public_tmp__ is Linux-only; the caller treats (_, false) as "cannot
// determine ownership" and fails closed. Windows is a build-only CI gate.
func fileOwnerUID(_ os.FileInfo) (uint32, bool) {
	return 0, false
}
