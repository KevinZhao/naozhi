//go:build !windows

package ccassets

import (
	"os"
	"syscall"
)

// openNoFollow opens path read-only with O_NOFOLLOW so the kernel atomically
// refuses a final-component symlink, closing the TOCTOU window described in
// readCapped. naozhi's production target is Linux. [R202606d-SEC-1]
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
