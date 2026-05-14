package session

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/eventlog/persist"
)

// orphanSweepAge is how stale a <keyhash>.log file must be (mtime-wise)
// before we consider it an orphan. 30 days is a conservative default:
//
//   - long enough that a user returning to a seldom-used session after
//     two weeks of absence still has their history intact
//   - short enough that sessions.json drift after a stale naozhi
//     deployment doesn't accumulate dozens of GB of abandoned logs
//
// The time value is intentionally longer than attachment refTTL
// (30d by default) so an attachment that's still reachable via
// event-log ImagePaths can never be orphaned before the attachment
// itself is pruned.
const orphanSweepAge = 30 * 24 * time.Hour

// sweepOrphanEventLogs removes <keyhash>.log / .idx files in the
// event-log directory whose keyhash is not present in the given set
// AND whose mtime is older than orphanSweepAge.
//
// Called once during NewRouter after the session map has been
// reconstructed from sessions.json — at that point the set of
// "known" keyhashes is authoritative, and any .log file whose stem
// does not appear there is either:
//
//   - A session the operator deleted via DELETE /api/sessions but
//     whose DropKey call failed (partial cleanup). These are
//     identified by being stale — a live Router would have a fresh
//     mtime on the file from the most recent event flush.
//   - A session restored on a different naozhi deployment and
//     accidentally left in this deployment's events/. Also stale.
//   - A session whose sessions.json record was removed manually
//     (rm -rf ~/.naozhi/sessions.json then restart). We keep these
//     files if they are recent — the operator may be in the middle
//     of a migration, and silently deleting their data would be
//     very bad.
//
// Returns (removed, err). An err only arises when we cannot read
// the directory at all; individual file failures are logged and
// skipped so a single permission-denied entry doesn't halt the
// sweep.
//
// Safe to call with a nil persister — we read the directory
// directly without touching the persister's live writer map.
func sweepOrphanEventLogs(dir string, knownKeys map[string]struct{}, now time.Time) (int, error) {
	if dir == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
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
		// Only eat .log / .idx files with our known stem shape. tmp
		// files are handled separately by persist.SweepOrphans which
		// is called inside NewPersister.
		if !persist.IsLogFileName(name) && !persist.IsIdxFileName(name) {
			continue
		}
		// Strip the extension to get the bare keyhash stem.
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		if _, known := knownStems[stem]; known {
			continue
		}
		// Unknown stem → check age before deleting.
		info, err := entry.Info()
		if err != nil {
			slog.Warn("eventlog orphan sweep: stat failed", "file", name, "err", err)
			continue
		}
		if info.ModTime().After(cutoff) {
			// Recent file with unknown stem — operator may be
			// mid-migration. Log once so the log shows up in audits
			// but do not delete.
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

// runOrphanSweep runs sweepOrphanEventLogs in a background goroutine
// and logs the result. Called once from NewRouter after the session
// map is populated. Non-fatal: any error is logged but does not
// block startup.
//
// The sweep runs in a goroutine (rather than inline) because
// directory walks over thousands of sessions on slow storage (NFS,
// spinning disks) can take seconds; the Router should be up and
// serving dashboards immediately.
//
// historyWg-tracked so Shutdown waits for it — a sweep in progress
// holding an os.ReadDir walk is cheap to interrupt, but we want
// deterministic shutdown order for testability.
func (r *Router) runOrphanSweep() {
	if r.eventLogDir == "" {
		return
	}
	// Snapshot known keys under the read lock so we don't race with
	// concurrent session spawns.
	r.mu.RLock()
	known := make(map[string]struct{}, len(r.sessions))
	for k := range r.sessions {
		known[k] = struct{}{}
	}
	r.mu.RUnlock()

	r.historyWg.Add(1)
	go func() {
		defer r.historyWg.Done()
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
	}()
}
