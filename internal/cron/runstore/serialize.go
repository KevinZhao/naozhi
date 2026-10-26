package runstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// readRun parses a single run file. Returns ErrCorruptRun on parse failure
// or oversize; fs.ErrNotExist propagates unchanged.
//
// Open uses O_NOFOLLOW + Fstat on the resulting fd so the bytes read come
// from exactly the inode validated as a regular file — no Lstat→ReadFile
// TOCTOU in which runs/<jobID>/<runID>.json could be swapped for a symlink
// (#827). This guard matters because Get() takes a caller-supplied runID that
// was never ReadDir-filtered; scan-based callers use readRunNoLstat instead.
func (s *Store) readRun(path string) (*CronRun, error) {
	// openRunFile opens with O_NOFOLLOW (open_unix.go).
	f, err := openRunFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Fstat describes the exact open inode. O_NOFOLLOW already refused symlinks,
	// but a fifo/socket/device with the right name only fails here.
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: not a regular file", ErrCorruptRun)
	}
	return s.parseRunFromFile(f, fi)
}

// readRunNoLstat is the loop-friendly variant of readRun for callers whose
// directory scan already rejected symlinks / non-regular entries via
// DirEntry.Type(); it skips the redundant fd validation.
//
// SAFETY: must NOT be used for a constructed path such as Get()'s
// caller-supplied runID — readRun's O_NOFOLLOW guard is the only barrier
// against runs/<jobID>/<runID>.json being a symlink to a sensitive file.
func (s *Store) readRunNoLstat(path string) (*CronRun, error) {
	return s.parseRunBytes(path)
}

// parseRunFromFile reads the open fd's contents (bounded by maxRunBytes+1
// so we can detect oversize without slurping arbitrary bytes) and decodes
// the JSON. Used by readRun where the fd is the TOCTOU-safe handle. fi is
// the Fstat result already validated as a regular file, used to size-hint
// the buffer.
func (s *Store) parseRunFromFile(f *os.File, fi os.FileInfo) (*CronRun, error) {
	// Preallocate from fi.Size(); decodeRunBytes still enforces the cap on the
	// returned length, so a file that grew between Stat and Read is rejected.
	size := fi.Size()
	if size < 0 || size > s.maxRunBytes {
		// Over cap per Stat: short-circuit; same wrap as parseRunBytes so the error
		// shape is identical.
		return nil, fmt.Errorf("%w: %d bytes > %d cap", ErrCorruptRun, size, s.maxRunBytes)
	}
	buf := make([]byte, 0, size)
	data, err := readAllInto(f, buf)
	if err != nil {
		return nil, err
	}
	return decodeRunBytes(data, s.maxRunBytes)
}

// zeroProgressLimit is how many consecutive (0, nil) reads readAllIntoReader
// tolerates before it gives up with io.ErrNoProgress.
const zeroProgressLimit = 2

// readAllIntoReader is the testable core of readAllInto. It accepts an
// io.Reader so tests can inject a reader that repeatedly returns (0, nil).
//
// The guard returns io.ErrNoProgress after zeroProgressLimit consecutive
// (0, nil) reads so the loop cannot hang on readers that are contractually
// allowed to do that (e.g. some FUSE file systems); os.File on Linux does
// not in practice.
func readAllIntoReader(r io.Reader, buf []byte) ([]byte, error) {
	zeroCount := 0
	for {
		if len(buf) == cap(buf) {
			buf = append(buf, 0)[:len(buf)]
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buf, nil
			}
			return buf, err
		}
		if n == 0 {
			zeroCount++
			if zeroCount >= zeroProgressLimit {
				return buf, io.ErrNoProgress
			}
		} else {
			zeroCount = 0
		}
	}
}

// decodeRunBytes enforces the size cap and json.Unmarshal step shared by the
// file-based and bytes-based read paths so both wrap errors identically.
func decodeRunBytes(data []byte, maxRunBytes int64) (*CronRun, error) {
	if int64(len(data)) > maxRunBytes {
		return nil, fmt.Errorf("%w: %d bytes > %d cap", ErrCorruptRun, len(data), maxRunBytes)
	}
	var run CronRun
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorruptRun, err)
	}
	return &run, nil
}
