// Package naozhilog implements history.Source backed by naozhi's
// own on-disk event log (internal/eventlog/persist). It is the
// "local" tier in MergedSource; the "fallback" tier is the
// Claude-CLI JSONL reader in internal/history/claudejsonl.
//
// Data flow (read path):
//
//	session.Router.EventEntriesBeforeCtx
//	  → merged.Source.LoadBefore
//	    → naozhilog.Source.LoadBefore  (this file)
//	      → persist.ReadAllIdx + log frame decode
//	    → claudejsonl.Source.LoadBefore  (fallback)
//
// naozhilog stores cli.EventEntry records in their persistence-layer
// envelope (schema.Record wrapping EventEntry JSON). The read path
// reverses the framing, decodes the envelope, and returns
// cli.EventEntry slices to callers so they can be fed back through
// ManagedSession.InjectHistory without further translation.
//
// Design constraints (from RFC §3.4):
//   - Concurrency-safe with the writer side: readers open their own
//     read-only fds and tolerate partial tails via the framing layer.
//   - Missing / empty files are NOT errors; they return empty slices
//     so the MergedSource fallback path is uniform.
//   - Version-mismatched files (schema.WireVersion drift) are
//     REFUSED, not salvaged, so upgrades can't silently parse a
//     subset of the new format.
package naozhilog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// Source is an in-package history.Source backed by
// <keyhash>.log / <keyhash>.idx under a single directory. Constructed
// per-session so LoadLatest / LoadBefore share the same key→path
// resolution without a parameter each time.
type Source struct {
	dir string
	key string
}

// New builds a Source for dir and a specific session key. The key
// is used to derive the <keyhash>.log path; no file access happens
// until LoadLatest / LoadBefore is called.
//
// dir="" is a valid configuration — it disables reads entirely so a
// naozhi deployment that opts out of event-log persistence can keep
// using router.attachHistorySource unchanged. Both LoadLatest and
// LoadBefore return (nil, nil) for a disabled source.
func New(dir, key string) *Source {
	return &Source{dir: dir, key: key}
}

// LoadLatest returns up to `limit` newest entries from the session's
// log file, in chronological order.
//
// Implementation:
//  1. Use the idx sparse index's last-entry TimeMS as a seed "before"
//     value, then walk backwards through idx + log bytes. In practice
//     for LoadLatest (limit ≈ 500) we just do a full log scan +
//     tail-cut — the log caps at ~100 MiB rotate-protected, so a full
//     scan is still < 200ms on ext4/gp3. Keeping the code simple
//     beats a marginal speedup here; LoadBefore handles the hot
//     pagination case with idx-driven seeking.
//  2. Decode every record; keep `limit` newest.
//
// Called once per session at startup by session.Router to pre-fill
// persistedHistory, so its performance budget is "20-50ms per
// session × number of resurrected sessions" — acceptable.
func (s *Source) LoadLatest(ctx context.Context, limit int) ([]cli.EventEntry, error) {
	if s == nil || s.dir == "" || s.key == "" || limit <= 0 {
		return nil, nil
	}
	all, err := s.readAllEntries(ctx)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, nil
}

// LoadBefore returns up to `limit` entries strictly older than
// beforeMS (unix ms), in chronological order. Used by the dashboard
// "load earlier" pagination path via MergedSource.LoadBefore.
//
// Implementation (current):
//   - Full-scan via readAllEntries, then filter to records with
//     Time < beforeMS and keep the `limit` newest. This is a plain
//     linear pass over the whole log file — the same cost as
//     LoadLatest — and is acceptable while logs are rotate-capped at
//     ~100 MiB (< 200ms scan on ext4/gp3).
//   - TODO: use the idx sparse index to seek near beforeMS and walk
//     forward from there, avoiding the full-file scan on large logs.
//     Tracked separately; not implemented here.
//
// beforeMS <= 0 is treated as "no upper bound" and falls through to
// LoadLatest's behavior.
func (s *Source) LoadBefore(ctx context.Context, beforeMS int64, limit int) ([]cli.EventEntry, error) {
	if s == nil || s.dir == "" || s.key == "" || limit <= 0 {
		return nil, nil
	}
	if beforeMS <= 0 {
		return s.LoadLatest(ctx, limit)
	}
	all, err := s.readAllEntries(ctx)
	if err != nil {
		return nil, err
	}
	// Filter by timestamp.
	filtered := all[:0]
	for _, e := range all {
		if e.Time < beforeMS {
			filtered = append(filtered, e)
		}
	}
	if len(filtered) == 0 {
		return nil, nil
	}
	if len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:]
	}
	return filtered, nil
}

// readAllEntries decodes every record from the log file (skipping
// the header) and returns the resulting EventEntry slice in log
// order (= chronological, by construction of the persist layer).
//
// Missing file → (nil, nil). Corrupt / unsupported-version file →
// (nil, nil) + slog.Warn; the caller falls back to Claude JSONL.
// We never surface the error to the caller because that would
// prevent the fallback from running.
func (s *Source) readAllEntries(ctx context.Context) ([]cli.EventEntry, error) {
	path := persist.LogPath(s.dir, s.key)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open naozhi log %s: %w", path, err)
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 64*1024)
	// Pre-allocate to the in-memory ring upper bound so the per-record
	// append loop (up to ~500 entries) avoids the repeated doubling
	// reallocations of a nil-start slice. 512 ≈ persistedHistory ring cap
	// (LoadLatest's limit ≈ 500); over-shooting reads still grow normally.
	// R20260530-PERF-3.
	const estEntries = 512
	out := make([]cli.EventEntry, 0, estEntries)

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rec, err := persist.ReadRecord(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if errors.Is(err, persist.ErrPartialTail) {
				// Writer is mid-write; stop gracefully.
				break
			}
			if errors.Is(err, schema.ErrUnsupportedVersion) {
				slog.Warn("naozhilog: unsupported wire version; skipping file",
					"path", path, "err", err)
				return nil, nil
			}
			// Any other decode error means the file is corrupt.
			// Log and stop; return what we have so partial reads
			// still provide value.
			slog.Warn("naozhilog: decode error; truncating read",
				"path", path, "err", err)
			break
		}

		// Skip header record.
		if rec.Type == schema.TypeHeader {
			continue
		}
		if len(rec.Entry) == 0 {
			continue
		}

		var entry cli.EventEntry
		if err := json.Unmarshal(rec.Entry, &entry); err != nil {
			slog.Warn("naozhilog: entry JSON decode failed; skipping",
				"path", path, "seq", rec.Seq, "err", err)
			continue
		}
		// Every persisted entry must have a UUID (stampUUID runs in
		// cli.EventLog.Append before the record reaches disk). A
		// missing UUID post-decode flags a producer bug or a
		// hand-edited file; we still emit the entry because dropping
		// it would lose history, but MergedSource dedup cannot anchor
		// against it, so log a warning once per file.
		if entry.UUID == "" {
			slog.Warn("naozhilog: entry missing UUID post-decode; dedup may regress",
				"path", path, "seq", rec.Seq, "time", entry.Time)
		}
		out = append(out, entry)
	}
	return out, nil
}
