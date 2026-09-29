package sandboxstore

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/osutil/jsonfile"
)

// Pending is the in-flight record written to <root>/sandboxpending/<runID>.json
// before a sandbox run is invoked and removed once the run is terminal. It
// survives a naozhi restart: the held stream dies with the process but the
// microVM keeps running, and this file is the next boot's only handle to Stop
// it. Which records are valid, and what to do with them, is cron's call.
type Pending struct {
	JobID            string `json:"job_id"`
	RunID            string `json:"run_id"`
	RuntimeSessionID string `json:"runtime_session_id"`
	StartedAtMS      int64  `json:"started_at_ms"`
}

// maxPendingRecordBytes caps one pending record: two ids, a session id and a
// timestamp, so 32 KiB bounds what a tampered file can allocate.
const maxPendingRecordBytes = 32 << 10

func (st Store) pendingDir() string { return st.Subtree("sandboxpending") }

// WritePending persists p and returns the file's path for the paired remove,
// or "" when persistence is off or the write failed. Best-effort: without the
// file a restart cannot Stop the run, which then ends at the platform's
// lifetime bound.
func (st Store) WritePending(p Pending, lg *slog.Logger) string {
	dir := st.pendingDir()
	if dir == "" {
		return ""
	}
	if !validID(p.RunID) {
		lg.Warn("cron sandbox: pending write rejected non-hex run id", "run_id", p.RunID)
		return ""
	}
	// Symlink-guarded: a planted `<root>/sandboxpending → /elsewhere` must not
	// redirect the restart handle (#2166).
	if err := st.MkdirSubtree(dir); err != nil {
		lg.Warn("cron sandbox: pending dir create failed; restart reconcile unavailable for this run", "err", err)
		return ""
	}
	b, err := json.Marshal(p)
	if err != nil {
		lg.Warn("cron sandbox: pending marshal failed", "err", err)
		return ""
	}
	path := filepath.Join(dir, p.RunID+".json")
	// Atomic: a record truncated by a crash mid-write would be dropped as
	// corrupt, leaving the run a permanent orphan.
	if err := osutil.WriteFileAtomic(path, b, 0o600); err != nil {
		lg.Warn("cron sandbox: pending write failed; restart reconcile unavailable for this run", "err", err)
		return ""
	}
	return path
}

// errNotPending reports a path outside the pending directory.
var errNotPending = errors.New("cron sandbox: not a pending record path")

// isPendingPath reports whether path names a record directly in the pending
// directory, so RemovePending and ReadPending cannot be pointed elsewhere.
func (st Store) isPendingPath(path string) bool {
	dir := st.pendingDir()
	return dir != "" && filepath.Dir(path) == dir && strings.HasSuffix(path, ".json")
}

// RemovePending deletes a pending record. A missing file (already removed)
// is not an error; "" is a no-op.
func (st Store) RemovePending(path string) error {
	if path == "" {
		return nil
	}
	if !st.isPendingPath(path) {
		return errNotPending
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// PendingState says what reading a pending record found.
type PendingState int

const (
	// PendingOK: Rec holds the decoded record.
	PendingOK PendingState = iota
	// PendingGone: the file is gone (its run finished meanwhile).
	PendingGone
	// PendingUnreadable: the read failed (I/O error, over the size cap, a
	// symlink); the file is still there.
	PendingUnreadable
	// PendingCorrupt: the file exists but does not decode.
	PendingCorrupt
)

// ReadPending reads the record at path. Bounded and symlink-refusing; a
// corrupt record is left in place for the caller to decide about.
func (st Store) ReadPending(path string) (Pending, PendingState) {
	if !st.isPendingPath(path) {
		return Pending{}, PendingUnreadable
	}
	rec, outcome, err := jsonfile.Load[Pending](path, jsonfile.Options{
		MaxBytes: maxPendingRecordBytes,
		Label:    "cron sandbox pending record",
		Corrupt:  jsonfile.LeaveCorrupt,
	})
	switch {
	case err != nil:
		return Pending{}, PendingUnreadable
	case outcome == jsonfile.Absent:
		return Pending{}, PendingGone
	case outcome == jsonfile.Parsed:
		return rec, PendingOK
	default:
		return Pending{}, PendingCorrupt
	}
}

// PendingEntry is one record ListPending found.
type PendingEntry struct {
	// Name is the file name, for logs.
	Name  string
	Path  string
	Rec   Pending
	State PendingState
}

// ListPending reads every record in the pending directory. A missing
// directory is an empty list; a failed scan is an error.
func (st Store) ListPending() ([]PendingEntry, error) {
	dir := st.pendingDir()
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]PendingEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		rec, state := st.ReadPending(path)
		if state == PendingGone {
			continue
		}
		out = append(out, PendingEntry{Name: e.Name(), Path: path, Rec: rec, State: state})
	}
	return out, nil
}
