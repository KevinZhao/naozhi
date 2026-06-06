package cron

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// appendMarshalBufPool reuses bytes.Buffer + json.Encoder scratch space
// across runStore.Append calls so each Append avoids the per-call
// encodeState alloc that json.Marshal performs internally. Mirrors the
// MarshalRecord pattern in internal/eventlog/schema/record.go. Cron Append
// rate is bounded (≤ 1Hz × N jobs) but every persisted record allocates
// ~2KB of encode scratch otherwise — pooling drops that to amortised zero
// after the warmup period. R240-PERF-6 / #1043.
// appendMarshalScratch pairs a bytes.Buffer with the json.Encoder bound to
// it. R20260602190132-PERF-2: a json.Encoder caches its internal encodeState
// (~2KB) across Encode calls on the same encoder, but constructing a fresh
// json.NewEncoder(buf) every marshalRunPooled call discarded that cache so the
// pooled buffer never actually amortised the encoder-side alloc. Pooling the
// encoder alongside the buffer closes that gap — the encodeState is now reused
// for the lifetime of the pooled pair.
type appendMarshalScratch struct {
	buf *bytes.Buffer
	enc *json.Encoder
}

var appendMarshalBufPool = sync.Pool{
	New: func() any {
		buf := bytes.NewBuffer(make([]byte, 0, 4*1024))
		return &appendMarshalScratch{buf: buf, enc: json.NewEncoder(buf)}
	},
}

// appendMarshalPoolMaxCap drops oversized buffers from the pool so a
// one-off near-MaxRunRecordBytes record does not pin memory for the
// process lifetime. Sized at 2× MaxRunRecordBytes for headroom.
const appendMarshalPoolMaxCap = 2 * MaxRunRecordBytes

// marshalRunPooled encodes run via a pooled bytes.Buffer + json.Encoder.
// Returns a freshly-copied []byte (independent of the pooled buffer) so
// callers may retain it after the buffer is recycled. Behaviourally
// identical to json.Marshal(run) except for json.Encoder's trailing
// '\n' which is stripped to match Marshal output.
func marshalRunPooled(run *CronRun) ([]byte, error) {
	sc := appendMarshalBufPool.Get().(*appendMarshalScratch)
	buf := sc.buf
	defer func() {
		if buf.Cap() > appendMarshalPoolMaxCap {
			return
		}
		buf.Reset()
		appendMarshalBufPool.Put(sc)
	}()
	buf.Reset()
	// json.Marshal default — keep HTML-escape parity so on-disk bytes match
	// the legacy callers and any future Unmarshal of historical records is
	// indistinguishable from json.Marshal output. The encoder is bound to buf
	// once at construction and reused from the pool, retaining its encodeState.
	if err := sc.enc.Encode(run); err != nil {
		return nil, err
	}
	body := buf.Bytes()
	if n := len(body); n > 0 && body[n-1] == '\n' {
		body = body[:n-1]
	}
	out := make([]byte, len(body))
	copy(out, body)
	return out, nil
}

// readRun parses a single run file. Returns ErrCorruptRun on parse
// failure or oversize; fs.ErrNotExist propagates unchanged.
//
// R235-SEC-5 / R242-GO-17 / R238-SEC-7 (#827): the original implementation
// did Lstat + (cond) ReadFile, which left a TOCTOU window — between the
// Lstat result observing a regular file and ReadFile opening the path,
// an attacker with write access to runs/<jobID>/ could swap the entry
// for a symlink and have ReadFile dereference a sensitive file. We close
// the window by using OpenFile with O_NOFOLLOW (kernel refuses to follow
// a final-component symlink, returning ELOOP) and Fstat'ing the resulting
// fd: the bytes we read come from exactly the inode whose mode we just
// validated as a regular file, regardless of any concurrent rename. The
// guard is the only barrier between Get() and a malicious symlink because
// Get() takes a caller-supplied runID that has not been ReadDir-filtered.
// diskListNewestFirst / trimJobLocked already skip symlinks during their
// directory scans, so they use readRunNoLstat to avoid paying for the
// redundant fd validation.
func (s *runStore) readRun(path string) (*CronRun, error) {
	// openRunFile is platform-specialised: Unix uses O_NOFOLLOW for a
	// kernel-atomic symlink refusal; Windows falls back to a Lstat-then-
	// Open two-step (best-effort, since O_NOFOLLOW is Unix-only).
	f, err := openRunFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Fstat on the fd returns metadata for the exact inode we have
	// open — no second path lookup, no race window. Reject anything
	// that's not a plain file: Open with O_NOFOLLOW already filtered
	// symlinks, but a fifo/socket/device with the right name would
	// still get past Open and only Fstat catches it.
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: not a regular file", ErrCorruptRun)
	}
	return s.parseRunFromFile(f, fi)
}

// readRunNoLstat is the loop-friendly variant of readRun for callers that
// have already filtered the entry through DirEntry.Type() (rejecting symlinks
// + non-regular modes during the directory scan). It skips the redundant
// Lstat syscall, halving syscall count for large directory listings —
// R245-PERF-9 (cluster: R243-PERF-11).
//
// SAFETY: must NOT be used as the entry-point for a constructed path (e.g.
// Get()'s direct path lookup). Get arrives with a caller-supplied runID
// that has not been ReadDir-filtered, so the Lstat guard in readRun is the
// only barrier against `runs/<jobID>/<runID>.json` being a symlink to
// /etc/passwd. diskListNewestFirst is the sole caller because its scan loop
// already drops symlinks at the e.Type()&fs.ModeSymlink check.
func (s *runStore) readRunNoLstat(path string) (*CronRun, error) {
	return s.parseRunBytes(path)
}

// parseRunFromFile reads the open fd's contents (bounded by maxRunBytes+1
// so we can detect oversize without slurping arbitrary bytes) and decodes
// the JSON. Used by readRun where the fd is the TOCTOU-safe handle. fi is
// the Fstat result already validated as a regular file, used to size-hint
// the buffer.
func (s *runStore) parseRunFromFile(f *os.File, fi os.FileInfo) (*CronRun, error) {
	// io.ReadAll grows incrementally; preallocate when fi.Size() is a
	// reasonable hint. The cap+1 read pattern is irrelevant here because
	// decodeRunBytes enforces the cap explicitly on the returned slice
	// length, so even a regular file that grew between Stat and Read
	// gets rejected by the size check.
	size := fi.Size()
	if size < 0 || size > s.maxRunBytes {
		// Stat already says we're over cap — short-circuit before any
		// ReadAll alloc. Match parseRunBytes's wrap exactly so callers
		// can't tell the readRun vs readRunNoLstat path apart by error
		// shape.
		return nil, fmt.Errorf("%w: %d bytes > %d cap", ErrCorruptRun, size, s.maxRunBytes)
	}
	buf := make([]byte, 0, size)
	data, err := readAllInto(f, buf)
	if err != nil {
		return nil, err
	}
	return decodeRunBytes(data, s.maxRunBytes)
}

// readAllIntoReader is the testable core of readAllInto. It accepts an
// io.Reader so unit tests can inject a fake reader that repeatedly returns
// (0, nil) to exercise the zero-progress guard (R171023-CR-007).
//
// The guard breaks out of the loop after zeroProgressLimit consecutive
// (0, nil) reads so the function does not hang on io.Reader implementations
// that are contractually allowed to return (0, nil) (e.g., certain FUSE
// file systems). os.File on Linux follows POSIX and will not do this in
// practice, but defence-in-depth applies here.
const zeroProgressLimit = 2

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

// decodeRunBytes enforces the size cap and json.Unmarshal step shared by
// both file-based and bytes-based read paths. Extracted from parseRunBytes
// so parseRunFromFile (the TOCTOU-safe path) can reuse the wrapping shape
// without an extra ReadFile.
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
