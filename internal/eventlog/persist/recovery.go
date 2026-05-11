package persist

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// Recover brings a (<stem>.log, <stem>.idx) pair into a consistent
// state before the writer goroutine opens them for append.
//
// The invariants we enforce on exit:
//
//  1. Idx size is a multiple of schema.IdxEntrySize.
//     Anything less is a torn final write — we round down.
//  2. The log's on-disk byte length equals LAST_IDX_EDGE, where
//     LAST_IDX_EDGE = lastIdxEntry.ByteOff + lastIdxEntry.Len.
//     The strict write order (log.Sync → idx.Sync, see package doc)
//     means the log is at least as far along as the idx. Any bytes
//     past LAST_IDX_EDGE are idx-unbacked; recovery truncates them
//     because they may be a partial record the writer never finished
//     (framing layer would detect it, but removing avoids the ambiguity
//     for the next writer's byte counter).
//  3. If the idx claims an edge PAST the log, the idx is the anomaly.
//     This is the "idx ahead of log" pathological case the RFC's §3.2.4
//     write-ordering specifically prevents — it should be impossible
//     when writers obey that ordering, but if we see it (e.g. after
//     ext4 journal replay ordering surprise, or a naozhi version with
//     buggy write order), we walk idx backwards to find the first
//     entry whose edge is within the log and truncate idx there.
//
// No Recover step writes more bytes than it removes — recovery is
// strictly a truncation. A session that lost trailing events loses
// them permanently (by design: half-written records cannot be safely
// reconstructed).
//
// Returns the post-recovery (log size, next seq, last entry time) so
// the Persister can initialize perKeyWriter without re-reading the
// file. A missing pair returns (0, 1, 0, nil) — the caller treats
// this as "fresh file, seq starts at 1 (after the header which is
// seq=0)".
//
// Errors from file I/O surface as-is; Recover does NOT attempt to
// continue past them, because an I/O failure here could mask a
// far larger disk problem.
type RecoverResult struct {
	LogSize     int64  // post-truncation log size in bytes
	NextSeq     uint64 // the Seq the next appended entry should use
	LastTimeMS  int64  // timestamp of the last persisted entry (0 if none)
	HeaderValid bool   // true when the file already has a committed header
	Repaired    bool   // true when Recover made any truncation
}

// Recover opens log + idx at the given paths, aligns them, and
// closes the files. The caller opens fresh writers afterward.
func Recover(logPath, idxPath string) (*RecoverResult, error) {
	// Phase 1: align idx tail to IdxEntrySize boundary. A torn tail
	// IS a repair — callers rely on Repaired=true so alerting can
	// surface every non-clean recovery.
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
	// mergeRepaired unions repair flags so every path's returned
	// RecoverResult correctly reflects the phase-1 tail align.
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
		// Log file exists but idx is empty/missing. This happens on
		// the very first record's write window: log was extended past
		// 0 bytes before idx received its first write. We must NOT
		// trust the log in that state — idx is the source of truth
		// for "what's durably persisted". Truncate log to 0 so next
		// startup is a clean slate.
		slog.Warn("event log recovery: log has bytes but idx is empty; truncating log",
			"log", logPath, "log_size", logSize)
		if err := truncateFile(logPath, 0); err != nil {
			return nil, fmt.Errorf("truncate log to 0: %w", err)
		}
		return mergeRepaired(&RecoverResult{NextSeq: 1, Repaired: true}), nil

	case hasLast && !logExists:
		// Idx exists but log doesn't. The only way this happens is
		// operator-hand surgery (someone rm'd the log); the data is
		// irrecoverable. Drop the idx to match.
		slog.Warn("event log recovery: idx exists but log is missing; clearing idx",
			"idx", idxPath)
		if err := os.Remove(idxPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("clear idx: %w", err)
		}
		return mergeRepaired(&RecoverResult{NextSeq: 1, Repaired: true}), nil
	}

	// Both exist, both have data.
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
		// Log has trailing bytes idx doesn't back. These may be a
		// partial record (writer crashed mid-write after log.Sync but
		// before idx.Sync); truncate to the idx-backed edge.
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
		// Idx ahead of log — see case 3 in the Recover godoc. Walk idx
		// entries backwards to find the first one whose edge is still
		// within the log. Everything after that point is dropped.
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

// alignIdxTail rounds the idx file size down to the nearest
// IdxEntrySize multiple, discarding any partial tail. Returns true
// when a truncation actually occurred (i.e. the file had a torn
// trailing entry), so Recover can mark its RecoverResult.Repaired
// correctly.
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

// reconcileIdxAheadOfLog handles the "idx thinks we wrote more than
// log actually has" case. Walks idx entries backwards; stops at the
// first entry whose edge (ByteOff + Len) is ≤ logSize. Everything
// after that point is truncated off the idx.
//
// If no idx entry fits (they all point past log end), we wipe idx
// entirely and also truncate log to 0 — the persisted state is too
// inconsistent to be trustworthy.
func reconcileIdxAheadOfLog(logPath, idxPath string, logSize int64) (*RecoverResult, error) {
	entries, err := ReadAllIdx(idxPath)
	if err != nil {
		return nil, fmt.Errorf("read idx for reconcile: %w", err)
	}
	safeIdx := -1
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.ByteOff+int64(e.Len) <= logSize {
			safeIdx = i
			break
		}
	}
	if safeIdx < 0 {
		// No entry fits — wipe both.
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

	// Truncate idx to (safeIdx+1) entries and log to that entry's
	// edge.
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

// fileSize is a small helper that returns (size, exists, err). A
// nonexistent file yields (0, false, nil) rather than an error.
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

// truncateFile opens + truncates + closes, returning a clearer error
// than the bare os.Truncate (which silently succeeds on nonexistent
// files in some environments).
func truncateFile(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		return err
	}
	return f.Sync()
}

// SweepOrphans removes rotate-staging files (*.tmp.*) from dir. Called
// by Persister startup so any crash-during-rotate leftovers don't
// accumulate. Returns the number of files removed.
//
// Any non-tmp file is left alone — a sibling naozhi version or a
// future rotate format might legitimately store something else in
// events/, and an aggressive sweep would lose their data.
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
