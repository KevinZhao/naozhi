//go:build windows

package project

import "errors"

// mkfifoForTest is unsupported on windows; the caller skips on this platform.
func mkfifoForTest(string) error { return errors.New("mkfifo unsupported on windows") }
