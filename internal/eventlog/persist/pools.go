package persist

import (
	"bufio"
	"io"
	"os"
)

// logWriteBufSize is the bufio.Writer capacity per perKeyWriter.logFile.
// 64 KiB matches ReadFramedBody's reader buffer and absorbs a typical
// framed record (1-20 KiB) without spilling to a syscall mid-frame.
const logWriteBufSize = 64 * 1024

// acquireLogBuf returns a fresh *bufio.Writer of logWriteBufSize bound to
// file (or to logFileWriterHook's wrapper when a test installed one).
func acquireLogBuf(file *os.File) *bufio.Writer {
	var w io.Writer = file
	if logFileWriterHook != nil {
		w = logFileWriterHook(file)
	}
	return bufio.NewWriterSize(w, logWriteBufSize)
}

// logFileWriterHook is a test-only seam that interposes on the log fd under
// logBuf, e.g. to fail writes the way a full disk does. Always nil in
// production.
var logFileWriterHook func(*os.File) io.Writer
