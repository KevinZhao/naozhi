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
