package datadir

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
)

// stdio.go — a size cap for the files an init system redirects naozhi's
// stdout and stderr into (#2747).
//
// launchd's StandardOutPath and systemd's StandardOutput=append: open the file
// themselves and hand naozhi the fd, so the file is not under any directory a
// Pass gardens and its path is not even known here. The cap therefore works on
// fds 1 and 2 directly, which also means it can never touch a file naozhi was
// not given.
//
// Truncate, never rename. The init system holds the fd and nothing reopens it,
// so a renamed file keeps receiving every write under its new name. That is
// also why newsyslog cannot rotate these files: it rotates by rename and has no
// copytruncate. ftruncate(fd, 0) is safe only because the fd is O_APPEND, so the
// next write lands at the new end of file; on a non-append fd it would land at
// the old offset and leave a hole the size of everything dropped, so such fds
// are skipped.
//
// The newest lines are kept: the first cap runs at startup, which after a crash
// is exactly when the previous process's panic trace on stderr is worth
// reading. Lines written between reading that tail and the truncate are lost;
// for a size cap that is an acceptable price for not coordinating with every
// writer of the fd.

// StdioSkip says why CapStdio left a file alone. The zero value means it acted.
type StdioSkip string

const (
	StdioSkipDisabled    StdioSkip = "disabled"    // maxSize <= 0
	StdioSkipNotRegular  StdioSkip = "not-regular" // pipe, journald socket, tty
	StdioSkipUnderCap    StdioSkip = "under-cap"
	StdioSkipNotAppend   StdioSkip = "not-append" // e.g. systemd StandardOutput=file:
	StdioSkipUnsupported StdioSkip = "unsupported"
)

// StdioResult reports what one CapStdio call did.
type StdioResult struct {
	Skip        StdioSkip
	Truncated   bool
	BytesBefore int64 // size seen when deciding; set once the file is over the cap
	KeptBytes   int64 // tail written back, marker line excluded
}

// errStdioUnsupported is returned by fdFlags on platforms without fcntl.
var errStdioUnsupported = errors.New("datadir: stdio cap unsupported on this platform")

// stdioMaxTail bounds the kept tail however large the cap is.
const stdioMaxTail = 4 << 20

// CapStdio truncates f to its newest whole lines once it exceeds maxSize: it
// keeps at most keepTail bytes aligned to line boundaries, truncates to zero,
// and writes a marker line followed by that tail back through f. See the file
// comment for why it truncates rather than renames and which fds it skips.
func CapStdio(f *os.File, stream string, maxSize, keepTail int64) (StdioResult, error) {
	var res StdioResult
	if maxSize <= 0 {
		res.Skip = StdioSkipDisabled
		return res, nil
	}
	info, err := f.Stat()
	if err != nil {
		return res, fmt.Errorf("datadir: stat %s: %w", stream, err)
	}
	if !info.Mode().IsRegular() {
		res.Skip = StdioSkipNotRegular
		return res, nil
	}
	if info.Size() <= maxSize {
		res.Skip = StdioSkipUnderCap
		return res, nil
	}
	res.BytesBefore = info.Size()
	appendMode, readable, err := fdFlags(f)
	if errors.Is(err, errStdioUnsupported) {
		res.Skip = StdioSkipUnsupported
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("datadir: fcntl %s: %w", stream, err)
	}
	if !appendMode {
		res.Skip = StdioSkipNotAppend
		return res, nil
	}

	tail := readTail(f, readable, info.Size(), keepTail)
	if err := f.Truncate(0); err != nil {
		return res, fmt.Errorf("datadir: truncate %s: %w", stream, err)
	}
	res.Truncated = true
	res.KeptBytes = int64(len(tail))

	// The marker is a JSON log record so a JSON-format stdout stays parseable
	// line by line; it goes out in the same write as the tail.
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("stdio log truncated; older lines were dropped",
		"stream", stream, "bytes_before", res.BytesBefore, "kept_bytes", res.KeptBytes, "max", maxSize)
	buf.Write(tail)
	if _, err := f.Write(buf.Bytes()); err != nil {
		return res, fmt.Errorf("datadir: write %s tail: %w", stream, err)
	}
	slog.Info("stdio log truncated",
		"stream", stream, "bytes_before", res.BytesBefore, "kept_bytes", res.KeptBytes, "max", maxSize)
	return res, nil
}

// readTail returns the last keepTail bytes of f trimmed to whole lines, or nil
// when nothing can be read. Reading runs to the current end of file rather than
// to size, so lines appended since the stat are kept too.
func readTail(f *os.File, readable bool, size, keepTail int64) []byte {
	if keepTail <= 0 {
		return nil
	}
	r := f
	if !readable {
		rf, err := openReadable(f)
		if err != nil {
			return nil
		}
		defer rf.Close()
		r = rf
	}
	// Start one byte early: if that byte is a newline, the first line in the
	// window is whole and survives the trim below.
	start := max(size-keepTail-1, 0)
	data, err := io.ReadAll(io.NewSectionReader(r, start, 2*keepTail+1))
	if err != nil {
		return nil
	}
	if start > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return nil
		}
		data = data[i+1:]
	}
	return data[:bytes.LastIndexByte(data, '\n')+1]
}

// StdioTask returns a Sweeper task that caps f at maxSize, keeping
// min(maxSize/8, 4MB) of tail. A file it must leave alone for a reason the
// operator can fix (not O_APPEND) is reported once, not every sweep.
func StdioTask(f *os.File, stream string, maxSize int64) func() {
	keepTail := min(maxSize/8, stdioMaxTail)
	var warnOnce sync.Once
	return func() {
		res, err := CapStdio(f, stream, maxSize, keepTail)
		if err != nil {
			slog.Warn("stdio cap failed", "stream", stream, "err", err)
			return
		}
		if res.Skip == StdioSkipNotAppend {
			warnOnce.Do(func() {
				slog.Warn("stdio log is over its cap but not opened for append; leaving it alone",
					"stream", stream, "size", res.BytesBefore, "max", maxSize,
					"hint", "use systemd StandardOutput=append: or journal instead of file:")
			})
		}
	}
}
