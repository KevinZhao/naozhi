package session

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestRouter_Start_Idempotent locks in that Router.Start (the lifecycle
// hook extracted from NewRouter for R245-ARCH-46 / #906) is idempotent.
// startBackgroundLifecycle is guarded by startOnce (R20260607-ARCH-1), so
// multiple Start calls must not spawn redundant orphan sweeps or overwrite
// r.attachmentTracker (which would leak the first tracker's goroutine).
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
	trackerAfterNew := r.attachmentTracker

	// Subsequent Start calls must be no-ops: startOnce.Do skips the body.
	r.Start(context.Background())
	r.Start(context.Background())

	if r.attachmentTracker != trackerAfterNew {
		t.Error("Start called multiple times overwrote attachmentTracker — startOnce guard not working (R20260607-ARCH-1)")
	}
}

// TestRouter_Start_NoEventLogDir verifies that Start is safe to call
// when EventLogDir is unset — both runOrphanSweep and
// startAttachmentTracker short-circuit on the empty-dir guard, so
// neither a sweep goroutine nor an attachment tracker should be
// installed.
func TestRouter_Start_NoEventLogDir(t *testing.T) {
	tmp := t.TempDir()
	r := NewRouter(RouterConfig{
		MaxProcs:  4,
		TTL:       time.Hour,
		StorePath: filepath.Join(tmp, "sessions.json"),
	})
	t.Cleanup(r.Shutdown)

	r.Start(context.Background())

	if r.attachmentTracker != nil {
		t.Error("attachmentTracker should be nil when eventLogDir is unset")
	}
}
