package cron

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestNewScheduler_MkdirAllFailure_StillReturnsScheduler pins R040034-GO-9
// (#1395): when the parent dir of StorePath cannot be MkdirAll'd
// (read-only volume, EACCES, ENOTDIR mid-path) NewScheduler MUST still
// return a *Scheduler so the rest of the wireup stays in lockstep. The
// next saveMarshaledSeq will surface a runtime error; the Error-level
// boot log makes the failure auditable rather than silent.
//
// Skip on platforms that don't honour 0o500 dir perms (Windows test
// runners commonly do not).
func TestNewScheduler_MkdirAllFailure_StillReturnsScheduler(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("0o500 dir perms not enforced on windows")
	}
	if os.Getuid() == 0 {
		t.Skip("root bypasses unwritable-dir enforcement")
	}
	t.Parallel()
	tmp := t.TempDir()
	// Pre-create an unwritable parent so MkdirAll cannot create the
	// child directory we'd need for cron.json's parent.
	parent := filepath.Join(tmp, "ro")
	if err := os.MkdirAll(parent, 0o500); err != nil {
		t.Fatalf("setup MkdirAll(ro): %v", err)
	}
	t.Cleanup(func() {
		// Restore writable so cleanup can remove the tree.
		_ = os.Chmod(parent, 0o700)
	})

	storePath := filepath.Join(parent, "child", "cron.json")
	s := NewScheduler(SchedulerConfig{StorePath: storePath, MaxJobs: 5}, SchedulerDeps{})
	if s == nil {
		t.Fatalf("NewScheduler returned nil even though contract guarantees a *Scheduler")
	}
	// We don't call Start() because loadJobs would also ENOENT here.
	// The boot-time slog.Error is the operator's signal; the rest of
	// the wireup code continues on the assumption it can later log
	// per-mutation failures. This test pins only that the constructor
	// returns a non-nil Scheduler.
}
