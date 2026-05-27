//go:build windows

package server

import "os"

// fileOwnerUID is unimplementable on Windows: NTFS uses SIDs, not POSIX UIDs,
// and naozhi's __public_tmp__ pseudo-project is a Linux-only feature. The
// caller (isPublicTmpForeignPrivate) must treat (_, false) as "cannot
// determine ownership", which on Linux conservatively allows the file
// through — matching test-fake behaviour. naozhi production runs Linux;
// Windows is a build-only CI gate.
func fileOwnerUID(_ os.FileInfo) (uint32, bool) {
	return 0, false
}
