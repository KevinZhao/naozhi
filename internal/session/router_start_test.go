package session

import (
	"path/filepath"
	"testing"
	"time"
)

// TestRouter_Start_Idempotent locks in that startBackgroundLifecycle (the
// lifecycle hook extracted from NewRouter for R245-ARCH-46 / #906) is
// idempotent. It is guarded by startOnce (R20260607-ARCH-1), so repeated
// calls must not spawn redundant orphan sweeps or overwrite
// r.hist.tracker (which would leak the first tracker's goroutine).
func TestRouter_Start_Idempotent(t *testing.T) {
	tmp := t.TempDir()
	r := NewRouter(RouterConfig{
		MaxProcs:    4,
		TTL:         time.Hour,
		StorePath:   filepath.Join(tmp, "sessions.json"),
		EventLogDir: filepath.Join(tmp, "events"),
	})
	t.Cleanup(r.Shutdown)

	// NewRouter already consumed startOnce via startBackgroundLifecycle.
	// Capture the tracker pointer installed at construction time.
	trackerAfterNew := r.hist.tracker

	// Subsequent calls must be no-ops: startOnce.Do skips the body.
	r.startBackgroundLifecycle()
	r.startBackgroundLifecycle()

	if r.hist.tracker != trackerAfterNew {
		t.Error("startBackgroundLifecycle called multiple times overwrote attachmentTracker — startOnce guard not working (R20260607-ARCH-1)")
	}
}

// TestRouter_Start_NoEventLogDir verifies that startBackgroundLifecycle is
// safe to call when EventLogDir is unset — both runOrphanSweep and
// startAttachmentTracker short-circuit on the empty-dir guard, so neither a
// sweep goroutine nor an attachment tracker should be installed.
func TestRouter_Start_NoEventLogDir(t *testing.T) {
	tmp := t.TempDir()
	r := NewRouter(RouterConfig{
		MaxProcs:  4,
		TTL:       time.Hour,
		StorePath: filepath.Join(tmp, "sessions.json"),
	})
	t.Cleanup(r.Shutdown)

	r.startBackgroundLifecycle()

	if r.hist.tracker != nil {
		t.Error("attachmentTracker should be nil when eventLogDir is unset")
	}
}
