//go:build unix

package datadir

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// fdFlags reports whether f's open file description is O_APPEND and whether
// it can be read.
func fdFlags(f *os.File) (appendMode, readable bool, err error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return false, false, err
	}
	var fl int
	var ferr error
	if err := rc.Control(func(fd uintptr) { fl, ferr = unix.FcntlInt(fd, unix.F_GETFL, 0) }); err != nil {
		return false, false, err
	}
	if ferr != nil {
		return false, false, ferr
	}
	return fl&unix.O_APPEND != 0, fl&unix.O_ACCMODE != unix.O_WRONLY, nil
}

// openReadable opens a second, readable description of f's file. systemd's
// append: hands over a write-only fd; /proc/self/fd reopens the inode itself
// on Linux. Elsewhere (macOS /dev/fd duplicates the write-only fd) it fails and
// the cap keeps no tail.
func openReadable(f *os.File) (*os.File, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}
	var path string
	if err := rc.Control(func(fd uintptr) { path = fmt.Sprintf("/proc/self/fd/%d", fd) }); err != nil {
		return nil, err
	}
	rf, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	a, aerr := f.Stat()
	b, berr := rf.Stat()
	if aerr != nil || berr != nil || !os.SameFile(a, b) {
		rf.Close()
		return nil, errors.New("datadir: reopened fd is a different file")
	}
	return rf, nil
}
