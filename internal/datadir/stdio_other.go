//go:build !unix

package datadir

import "os"

func fdFlags(*os.File) (appendMode, readable bool, err error) {
	return false, false, errStdioUnsupported
}

func openReadable(*os.File) (*os.File, error) { return nil, errStdioUnsupported }
