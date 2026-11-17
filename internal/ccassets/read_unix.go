//go:build !windows

package ccassets

import (
	"os"
	"syscall"

	"github.com/naozhi/naozhi/internal/osutil"
)

// openNoFollow opens path read-only with O_NOFOLLOW so the kernel atomically
// refuses a final-component symlink (see readCapped), and accepts only a
// regular file (osutil.OpenRegular): a FIFO a same-user agent left under
// ~/.claude would otherwise block open(2) with no writer and pin the handler.
func openNoFollow(path string) (*os.File, error) {
	f, _, err := osutil.OpenRegular(path, 0)
	return f, err
}

// openRegular is the scan side's open: it still follows a final-component
// symlink (an agents/*.md linked in from a dotfiles repo is listed), but
// O_NONBLOCK keeps a FIFO from blocking the open and the Fstat refuses
// anything that is not a regular file, so one FIFO cannot hang every scan.
func openRegular(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, osutil.ErrNotRegular
	}
	return f, nil
}
