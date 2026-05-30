package cron

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestStart_AfterStopRejected covers R249-ARCH-19 (#984): a Scheduler is
// single-use. Stop() cancels stopCtx and drains workers but does NOT reset
// the `started` latch, so a Start() after Stop() must NOT silently slide
// through the idempotency no-op branch and return nil — that would falsely
// signal "started OK" while the runner stays dead (cron.Start never
// re-invoked, stopCtx already cancelled). Start after Stop must return an
// explicit error so the misuse is visible.
func TestStart_AfterStopRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cron.json")

	s := NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: 10})
	if err := s.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	s.Stop()

	err := s.Start()
	if err == nil {
		t.Fatalf("Start after Stop returned nil; expected an error refusing the restart")
	}
	if !strings.Contains(err.Error(), "after Stop") {
		t.Fatalf("Start-after-Stop error = %q; want it to mention the Stop ordering", err)
	}
}

// TestStart_DoubleStartStillIdempotent guards against the #984 fix
// over-reaching: a plain double Start() (no intervening Stop) must remain a
// silent nil no-op, not an error.
func TestStart_DoubleStartStillIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cron.json")

	s := NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: 10})
	if err := s.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer s.Stop()
	if err := s.Start(); err != nil {
		t.Fatalf("second Start (no Stop) should be a nil no-op, got: %v", err)
	}
}
