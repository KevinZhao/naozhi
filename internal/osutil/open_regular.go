package osutil

import (
	"errors"
	"os"
)

// ErrNotRegular: OpenRegular refused a path that is not a regular file
// (FIFO, socket, device, directory).
var ErrNotRegular = errors.New("osutil: not a regular file")

// ErrTooLarge: the file exceeds the size cap passed to OpenRegular.
var ErrTooLarge = errors.New("osutil: file exceeds size cap")

// checkRegular vets an already-open f through its own fd, closing it on refusal.
func checkRegular(f *os.File, maxBytes int64) (*os.File, os.FileInfo, error) {
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, ErrNotRegular
	}
	if maxBytes > 0 && fi.Size() > maxBytes {
		f.Close()
		return nil, nil, ErrTooLarge
	}
	return f, fi, nil
}

// ErrNotDir: OpenDirIn refused a path that is not a real directory.
var ErrNotDir = errors.New("osutil: not a directory")

// OpenRegularIn is OpenRegular for rel inside root: no component may leave
// root (os.Root refuses it, a symlinked middle directory included), and the
// final one must be a regular file, not a symlink. root follows a symlink
// that stays inside it, so the Lstat before the open and the SameFile after
// it stand in for O_NOFOLLOW. The open does not block on a FIFO.
func OpenRegularIn(root *os.Root, rel string, maxBytes int64) (*os.File, os.FileInfo, error) {
	lst, err := root.Lstat(rel)
	if err != nil {
		return nil, nil, err
	}
	if !lst.Mode().IsRegular() {
		return nil, nil, ErrNotRegular
	}
	afterLstatIn()
	f, err := root.OpenFile(rel, openInFlags, 0)
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

// afterLstatIn runs between OpenRegularIn's Lstat and its open: the window
// a swapped file would use, which tests widen through it.
var afterLstatIn = func() {}

// OpenDirIn opens the directory rel inside root under OpenRegularIn's
// rules: a FIFO, a symlink or a file there fails at once (ErrNotDir).
func OpenDirIn(root *os.Root, rel string) (*os.File, error) {
	lst, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !lst.IsDir() {
		return nil, ErrNotDir
	}
	f, err := root.OpenFile(rel, openInFlags|openDirFlag, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.IsDir() || !os.SameFile(lst, fi) {
		f.Close()
		if err == nil {
			err = ErrNotDir
		}
		return nil, err
	}
	return f, nil
}
