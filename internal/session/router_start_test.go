package session

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestRouter_Start_Idempotent locks in that Router.Start (the lifecycle
// hook extracted from NewRouter for R245-ARCH-46 / #906) can be invoked
// repeatedly without panicking. Production callers invoke it
// transitively via NewRouter; future refactors that defer Start to a
// later moment must not regress this contract — re-calling Start after
// Shutdown, or calling it twice during boot, has to be a no-op shape.
func TestRouter_Start_Idempotent(t *testing.T) {
	tmp := t.TempDir()
	r := NewRouter(RouterConfig{
		MaxProcs:    4,
		TTL:         time.Hour,
		StorePath:   filepath.Join(tmp, "sessions.json"),
		EventLogDir: filepath.Join(tmp, "events"),
	})
	t.Cleanup(r.Shutdown)

	// NewRouter already invoked Start once via startBackgroundLifecycle.
	// A second explicit Start must not panic. Exact semantics (whether a
	// second sweep goroutine is scheduled) are deliberately unspecified
	// at this level — callers that want stricter "exactly-once" wiring
	// should layer their own gate on top.
	r.Start(context.Background())
	r.Start(context.Background())
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
