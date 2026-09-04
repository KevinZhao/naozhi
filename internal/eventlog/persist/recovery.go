package persist

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// RecoverResult is what Recover reports so the Persister can initialise a
// perKeyWriter without re-reading the file. A missing pair yields
// (LogSize 0, NextSeq 1): fresh file, the header takes seq=0.
type RecoverResult struct {
	LogSize     int64  // post-truncation log size in bytes
	NextSeq     uint64 // the Seq the next appended entry should use
	LastTimeMS  int64  // timestamp of the last persisted entry (0 if none)
	HeaderValid bool   // true when the file already has a committed header
	Repaired    bool   // true when Recover made any truncation
}

// maxIdxEntryLen is the largest framed length an IdxEntry.Len may carry:
// body cap + longest tolerated length prefix + two newlines. Anything
// beyond is corruption, not a large record.
const maxIdxEntryLen = int64(schema.MaxRecordBytes) + maxLengthDigits + 2

// idxEntrySane reports whether e's offset/length could have been produced
// by a writer. schema.UnmarshalIdxEntry casts uint32->int32 / uint64->int64
// unchecked, so a flipped high bit yields a NEGATIVE Len or ByteOff;
// recovery uses ByteOff+Len as the truncation edge, so Len=-1 would slice
// a committed record in half silently and Len=MinInt32 makes ftruncate
// fail EINVAL (#1817).
func idxEntrySane(e schema.IdxEntry) bool {
	if e.ByteOff < 0 || e.Len < 0 {
		return false
	}
	if int64(e.Len) > maxIdxEntryLen {
		return false
	}
	// Guard the addition so a huge ByteOff cannot overflow to a negative edge.
	return e.ByteOff <= math.MaxInt64-int64(e.Len)
}

// Recover brings a (<stem>.log, <stem>.idx) pair into a consistent state
// and closes the files; the caller opens fresh writers. Invariants on exit:
//  1. Idx size is a multiple of schema.IdxEntrySize (torn tail rounded down).
//  2. Log length == lastIdx.ByteOff + lastIdx.Len: the log.Sync → idx.Sync
//     write order makes bytes past that edge idx-unbacked (possibly a
//     partial record), so they are truncated.
//  3. If the idx edge lies PAST the log (impossible under that order), idx
//     is walked backwards to the newest entry within the log.
//
// Recovery only truncates, never reconstructs; I/O errors surface as-is.
func Recover(logPath, idxPath string) (*RecoverResult, error) {
	// Phase 1: align idx tail. A torn tail IS a repair — Repaired=true
	// drives alerting on every non-clean recovery.
	idxAligned, err := alignIdxTail(idxPath)
	if err != nil {
		return nil, fmt.Errorf("align idx: %w", err)
	}

	// Phase 2: determine idx-backed safe edge.
	last, hasLast, err := LastIdxEntry(idxPath)
	if err != nil {
		return nil, fmt.Errorf("load last idx: %w", err)
	}

	// Phase 3: reconcile against log size.
	logSize, logExists, err := fileSize(logPath)
	if err != nil {
		return nil, fmt.Errorf("stat log: %w", err)
	}
	mergeRepaired := func(res *RecoverResult) *RecoverResult {
		if idxAligned {
			res.Repaired = true
		}
		return res
	}

	switch {
	case !hasLast && !logExists:
		// Fresh install for this session.
		return mergeRepaired(&RecoverResult{NextSeq: 1}), nil

	case !hasLast && logExists:
		// Log has bytes but idx is empty: crashed inside the first record's
		// write window. idx is the source of truth for what is durable, so
		// truncate the log to 0.
		slog.Warn("event log recovery: log has bytes but idx is empty; truncating log",
			"log", logPath, "log_size", logSize)
		if err := truncateFile(logPath, 0); err != nil {
			return nil, fmt.Errorf("truncate log to 0: %w", err)
		}
		return mergeRepaired(&RecoverResult{NextSeq: 1, Repaired: true}), nil

	case hasLast && !logExists:
		// Idx without log only happens via operator hand surgery; the data
		// is irrecoverable, drop the idx to match.
		slog.Warn("event log recovery: idx exists but log is missing; clearing idx",
			"idx", idxPath)
		if err := os.Remove(idxPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("clear idx: %w", err)
		}
		return mergeRepaired(&RecoverResult{NextSeq: 1, Repaired: true}), nil
	}

	// Both exist. A corrupt last entry cannot yield a truncation edge; the
	// backwards walk skips insane entries and lands on the newest
	// trustworthy one.
	if !idxEntrySane(last) {
		slog.Warn("event log recovery: last idx entry is corrupt; backing off to newest sane entry",
			"log", logPath, "idx", idxPath,
			"byte_off", last.ByteOff, "len", last.Len)
		res, err := reconcileIdxAheadOfLog(logPath, idxPath, logSize)
		if err != nil {
			return nil, err
		}
		return mergeRepaired(res), nil
	}

	edge := last.ByteOff + int64(last.Len)

	switch {
	case edge == logSize:
		// Perfect alignment; idx exactly describes the log.
		return mergeRepaired(&RecoverResult{
			LogSize:     logSize,
			NextSeq:     last.Seq + 1,
			LastTimeMS:  last.TimeMS,
			HeaderValid: true,
		}), nil

	case edge < logSize:
		// Trailing bytes idx does not back (crash after log.Sync, before
		// idx.Sync): truncate to the idx-backed edge.
		slog.Info("event log recovery: truncating log tail beyond idx edge",
			"log", logPath, "log_size", logSize, "idx_edge", edge,
			"trimmed_bytes", logSize-edge)
		if err := truncateFile(logPath, edge); err != nil {
			return nil, fmt.Errorf("truncate log: %w", err)
		}
		return mergeRepaired(&RecoverResult{
			LogSize:     edge,
			NextSeq:     last.Seq + 1,
			LastTimeMS:  last.TimeMS,
			HeaderValid: true,
			Repaired:    true,
		}), nil

	default:
		// Idx ahead of log (invariant 3): walk idx backwards to the newest
		// entry whose edge is within the log.
		slog.Warn("event log recovery: idx ahead of log; backing off idx",
			"log", logPath, "idx", idxPath,
			"log_size", logSize, "idx_edge", edge)
		res, err := reconcileIdxAheadOfLog(logPath, idxPath, logSize)
		if err != nil {
			return nil, err
		}
		return mergeRepaired(res), nil
	}
}

// alignIdxTail rounds the idx file size down to an IdxEntrySize multiple,
// discarding a torn trailing entry. Returns true when it truncated so
// Recover can set Repaired.
func alignIdxTail(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	aligned := schema.AlignIdxSize(fi.Size())
	if aligned == fi.Size() {
		return false, nil
	}
	slog.Debug("event log recovery: idx tail not aligned; rounding down",
		"path", path, "size", fi.Size(), "aligned", aligned)
	if err := truncateFile(path, aligned); err != nil {
		return false, err
	}
	return true, nil
}

// reconcileIdxAheadOfLog walks idx entries backwards to the first sane one
// whose edge (ByteOff + Len) is <= logSize and truncates idx (and log, if
// longer) there. If no entry fits, both files are wiped — the persisted
// state is too inconsistent to trust.
func reconcileIdxAheadOfLog(logPath, idxPath string, logSize int64) (*RecoverResult, error) {
	entries, err := ReadAllIdx(idxPath)
	if err != nil {
		return nil, fmt.Errorf("read idx for reconcile: %w", err)
	}
	safeIdx := -1
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		// Sanity first: a negative Len makes ByteOff+Len < ByteOff, so the
		// <= logSize test would accept a corrupt entry and truncate the log
		// mid-record.
		if !idxEntrySane(e) {
			continue
		}
		if e.ByteOff+int64(e.Len) <= logSize {
			safeIdx = i
			break
		}
	}
	if safeIdx < 0 {
		slog.Warn("event log recovery: no idx entry fits within log; wiping both",
			"log", logPath, "idx", idxPath)
		if err := truncateFile(idxPath, 0); err != nil {
			return nil, fmt.Errorf("wipe idx: %w", err)
		}
		if err := truncateFile(logPath, 0); err != nil {
			return nil, fmt.Errorf("wipe log: %w", err)
		}
		return &RecoverResult{NextSeq: 1, Repaired: true}, nil
	}

	keepIdxBytes := int64(safeIdx+1) * schema.IdxEntrySize
	if err := truncateFile(idxPath, keepIdxBytes); err != nil {
		return nil, fmt.Errorf("truncate idx: %w", err)
	}
	safeEntry := entries[safeIdx]
	edge := safeEntry.ByteOff + int64(safeEntry.Len)
	if edge < logSize {
		if err := truncateFile(logPath, edge); err != nil {
			return nil, fmt.Errorf("truncate log to idx edge: %w", err)
		}
	}
	return &RecoverResult{
		LogSize:     edge,
		NextSeq:     safeEntry.Seq + 1,
		LastTimeMS:  safeEntry.TimeMS,
		HeaderValid: safeEntry.Seq == 0 || entries[0].Seq == 0,
		Repaired:    true,
	}, nil
}

// fileSize returns (size, exists, err); a nonexistent file is (0, false, nil).
func fileSize(path string) (int64, bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return fi.Size(), true, nil
}

// truncateFile opens + truncates + syncs. O_CREATE because
// reconcileIdxAheadOfLog may run against a log an operator just removed;
// ENOENT would fail the whole Persister startup. Mode 0o600 matches the
// Persister's file-creation perms so recovery never widens access.
func truncateFile(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		return err
	}
	return f.Sync()
}

// SweepOrphans removes rotate-staging files (*.tmp.*) from dir at Persister
// startup and returns the count. Non-tmp files are left alone: a sibling
// naozhi version may legitimately store something else in events/.
func SweepOrphans(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("read events dir %s: %w", dir, err)
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !IsTmpFileName(name) {
			continue
		}
		if err := os.Remove(dir + string(os.PathSeparator) + name); err != nil {
			slog.Warn("event log: failed to remove orphan tmp file",
				"path", name, "err", err)
			continue
		}
		removed++
	}
	return removed, nil
}
