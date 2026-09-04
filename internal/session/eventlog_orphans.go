package session

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/eventlog/persist"
)

// orphanSweepAge is how stale a <keyhash>.log file must be (mtime-wise)
// before it is considered an orphan. 30 days keeps a seldom-used session's
// history intact while bounding abandoned-log growth, and must not be
// shorter than attachment refTTL so an attachment reachable via event-log
// ImagePaths is never orphaned before the attachment itself is pruned.
const orphanSweepAge = 30 * 24 * time.Hour

// sweepOrphanEventLogs removes <keyhash>.log / .idx files in the event-log
// directory whose keyhash is not in knownKeys AND whose mtime is older than
// orphanSweepAge. Called once in NewRouter after the session map is rebuilt
// from sessions.json, when the known set is authoritative. Recent unknown
// files are kept: the operator may be mid-migration (sessions.json removed
// by hand) and silently deleting their data would be very bad.
//
// Returns (removed, err); err only when the directory itself is unreadable.
// Per-file failures are logged and skipped. Reads the directory directly, so
// it does not touch the persister's live writer map.
func sweepOrphanEventLogs(dir string, knownKeys map[string]struct{}, now time.Time) (int, error) {
	if dir == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}

	// Snapshot known stems so we don't rehash on every file.
	knownStems := make(map[string]struct{}, len(knownKeys))
	for k := range knownKeys {
		knownStems[persist.KeyHash(k)] = struct{}{}
	}

	cutoff := now.Add(-orphanSweepAge)
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		// tmp files are handled by persist.SweepOrphans inside NewPersister.
		if !persist.IsLogFileName(name) && !persist.IsIdxFileName(name) {
			continue
		}
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		if _, known := knownStems[stem]; known {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			slog.Warn("eventlog orphan sweep: stat failed", "file", name, "err", err)
			continue
		}
		if info.ModTime().After(cutoff) {
			// Recent unknown stem — operator may be mid-migration; log, keep.
			slog.Info("eventlog orphan sweep: unknown stem is recent, keeping",
				"file", name, "mtime", info.ModTime().UTC().Format(time.RFC3339))
			continue
		}
		fullPath := filepath.Join(dir, name)
		if err := os.Remove(fullPath); err != nil {
			slog.Warn("eventlog orphan sweep: remove failed",
				"file", name, "err", err)
			continue
		}
		removed++
	}
	return removed, nil
}

// runOrphanSweep runs sweepOrphanEventLogs in a background goroutine and logs
// the result. Called once from NewRouter after the session map is populated;
// errors are logged, never fatal. A goroutine because directory walks over
// thousands of sessions on slow storage can take seconds and the Router should
// serve immediately. historyWg-tracked so Shutdown order is deterministic.
func (r *Router) runOrphanSweep() {
	if r.eventLogDir == "" {
		return
	}
	// Snapshot known keys under the read lock so we don't race concurrent spawns.
	r.mu.RLock()
	known := make(map[string]struct{}, len(r.ss.sessions))
	for k := range r.ss.sessions {
		known[k] = struct{}{}
	}
	r.mu.RUnlock()

	// runHistoryTask takes historyWg.Add(1) under r.historyWgMu, atomic with
	// Shutdown's historyCancel(), so a Start after Shutdown cannot panic with
	// "WaitGroup is reused before previous Wait has returned" (#2186). It is a
	// no-op when historyCtx is already cancelled. The sweep ignores ctx.
	r.runHistoryTask(func(context.Context) {
		n, err := sweepOrphanEventLogs(r.eventLogDir, known, time.Now())
		if err != nil {
			slog.Warn("eventlog orphan sweep failed",
				"dir", r.eventLogDir, "err", err)
			return
		}
		if n > 0 {
			slog.Info("eventlog orphan sweep completed",
				"dir", r.eventLogDir, "removed", n)
		}
	})
}
