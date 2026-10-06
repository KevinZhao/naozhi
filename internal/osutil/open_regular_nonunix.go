//go:build !unix

package osutil

import "os"

// OpenRegular is the non-unix fallback of open_regular_unix.go, without
// O_NONBLOCK / O_NOFOLLOW: Lstat refuses a symlink or special file up front,
// and the opened fd must still be that same regular file.
func OpenRegular(path string, maxBytes int64) (*os.File, os.FileInfo, error) {
	lst, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !lst.Mode().IsRegular() {
		return nil, nil, ErrNotRegular
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	f, fi, err := checkRegular(f, maxBytes)
	if err != nil {
		return nil, nil, err
	}
	if !os.SameFile(lst, fi) {
		f.Close()
		return nil, nil, ErrNotRegular
	}
	return f, fi, nil
}

// openInFlags are OpenRegularIn's; there is no O_NONBLOCK or O_DIRECTORY
// here, so the Lstat before the open is the only special-file guard.
const (
	openInFlags = os.O_RDONLY
	openDirFlag = 0
)
