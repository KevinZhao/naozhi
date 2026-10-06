package runstore

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/naozhi/naozhi/internal/cron/sandboxstore"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// InflightMarker is the on-disk record for a local run in flight, at
// <root>/runinflight/<runID>.json. It is written when the run starts and
// removed when it reaches a terminal state, so one still present at boot names
// a run whose process died. What the next boot does with it is cron's call.
type InflightMarker struct {
	JobID       string                   `json:"job_id"`
	RunID       string                   `json:"run_id"`
	Trigger     runtelemetry.TriggerKind `json:"trigger,omitempty"`
	StartedAtMS int64                    `json:"started_at_ms"`
	Prompt      string                   `json:"prompt,omitempty"`
	WorkDir     string                   `json:"work_dir,omitempty"`
	Fresh       bool                     `json:"fresh,omitempty"`
	// Attempts counts boots that already tried to ADOPT this marker (#2712 PR
	// B). Zero (and absent, so pre-adoption markers need no migration) means
	// never tried; at cron's attempt cap the reconciler stops adopting and
	// records interrupted, so a marker that crashes its adoption costs at most
	// one extra boot, never a crash loop (#2751).
	Attempts int `json:"attempts,omitempty"`
	// SendWatermark is the session's SendWatermarker value just before Send
	// (#3104): it lets adoption accept a result that was already in the
	// replayed backlog. Absent on markers from older binaries and on runs
	// that never reached Send, which then adopt only a turn still running.
	SendWatermark string `json:"adopt_after,omitempty"`
}

// Markers reads and writes the run-inflight markers below Root, the cron state
// directory. It is rooted apart from Store on purpose: markers are written and
// reconciled even when run history is disabled (#2993). The zero value (Root
// "") has persistence disabled: writes do nothing and the list is empty.
type Markers struct {
	Root string
}

// Dir is the marker directory, or "" when persistence is disabled.
func (ms Markers) Dir() string {
	return datadir.FromRoot(ms.Root).Join("runinflight")
}

// Write persists m and returns its path, or "" when persistence is off or the
// write failed. Best-effort by design: a marker that cannot be written costs a
// missing history row on the next crash, which must not stop the run itself.
func (ms Markers) Write(m InflightMarker, lg *slog.Logger) string {
	dir := ms.Dir()
	if dir == "" {
		return ""
	}
	// Symlink-guarded create, the one guard every cron state subtree shares
	// (#2166): a planted `<root>/runinflight → /elsewhere` must not redirect
	// where the next boot looks, nor where this write lands.
	if err := (sandboxstore.Store{Root: ms.Root}).MkdirSubtree(dir); err != nil {
		lg.Warn("cron: run-inflight dir create failed; an interrupted run will not appear in history", "err", err)
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		lg.Warn("cron: run-inflight marshal failed", "err", err)
		return ""
	}
	// runID is scheduler-generated hex — path-safe by construction.
	path := filepath.Join(dir, m.RunID+".json")
	// Atomic: a truncated marker from a crash mid-write would be dropped as
	// corrupt at reconcile, which is the outcome the marker exists to prevent.
	if err := osutil.WriteFileAtomic(path, b, 0o600); err != nil {
		lg.Warn("cron: run-inflight write failed; an interrupted run will not appear in history", "err", err)
		return ""
	}
	return path
}

// Rewrite persists an updated marker in place at path, a path Write returned
// or List found. Reports success.
func (ms Markers) Rewrite(path string, m InflightMarker) bool {
	data, err := json.Marshal(m)
	if err != nil {
		slog.Warn("cron: run-inflight marker re-marshal failed", "path", path, "err", err)
		return false
	}
	if err := osutil.WriteFileAtomic(path, data, 0o600); err != nil {
		slog.Warn("cron: run-inflight marker rewrite failed", "path", path, "err", err)
		return false
	}
	return true
}

// Remove drops runID's marker. A missing marker is not an error; a failed
// remove is logged.
func (ms Markers) Remove(runID string) {
	dir := ms.Dir()
	if dir == "" || runID == "" {
		return
	}
	if err := os.Remove(filepath.Join(dir, runID+".json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("cron: run-inflight marker remove failed", "run_id", runID, "err", err)
	}
}

// RemovePath deletes the marker file at path. A missing file is not an error.
func (ms Markers) RemovePath(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Read loads one marker. ok=false for anything unusable: unreadable, not
// JSON, or missing the run id, job id or start time.
func (ms Markers) Read(path string) (InflightMarker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("cron: run-inflight marker read failed", "path", path, "err", err)
		return InflightMarker{}, false
	}
	var m InflightMarker
	if err := json.Unmarshal(b, &m); err != nil {
		slog.Warn("cron: run-inflight marker parse failed; dropping", "path", path, "err", err)
		return InflightMarker{}, false
	}
	if m.RunID == "" || m.JobID == "" || m.StartedAtMS <= 0 {
		slog.Warn("cron: run-inflight marker incomplete; dropping", "path", path)
		return InflightMarker{}, false
	}
	return m, true
}

// InflightEntry is one *.json file List found. OK is Read's verdict; an entry
// with OK false is unusable and still on disk.
type InflightEntry struct {
	Path   string
	Marker InflightMarker
	OK     bool
}

// List reads every *.json file in the marker directory, skipping
// subdirectories. A missing directory, or disabled persistence, is an empty
// list; a failed scan is an error.
func (ms Markers) List() ([]InflightEntry, error) {
	dir := ms.Dir()
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]InflightEntry, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		m, ok := ms.Read(path)
		out = append(out, InflightEntry{Path: path, Marker: m, OK: ok})
	}
	return out, nil
}
