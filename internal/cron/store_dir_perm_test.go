package cron

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestNewScheduler_ClampsStoreDirTo0700 covers issue #834 (R238-SEC-12):
// the cron data dir must be 0o700 at NewScheduler — not lazily on the
// first save — so the startup window does not let other local users
// list cron job IDs via directory enumeration.
func TestNewScheduler_ClampsStoreDirTo0700(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission semantics; skip on windows")
	}
	root := t.TempDir()
	// Inherit a deliberately loose 0o755 mode — this is the realistic
	// failure case (default XDG config dir mode on most distros).
	storeDir := filepath.Join(root, "cron-data")
	if err := os.MkdirAll(filepath.Dir(storeDir), 0o755); err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	if err := os.Mkdir(storeDir, 0o755); err != nil {
		t.Fatalf("seed loose dir: %v", err)
	}

	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(storeDir, "cron_jobs.json"),
		MaxJobs:   5,
	})
	defer s.Stop()

	// NewScheduler must have clamped the dir even before any save fires.
	fi, err := os.Stat(storeDir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	got := fi.Mode().Perm()
	if got != 0o700 {
		t.Errorf("storeDir perm = %#o, want 0o700 (clamp must happen at NewScheduler, not lazily on first save)", got)
	}
}

// TestNewScheduler_CreatesMissingStoreDir confirms the eager clamp also
// covers the fresh-deploy path: when the parent dir doesn't yet exist,
// NewScheduler creates it with 0o700.
func TestNewScheduler_CreatesMissingStoreDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission semantics; skip on windows")
	}
	root := t.TempDir()
	storeDir := filepath.Join(root, "fresh-deploy-data")
	// storeDir intentionally not created.

	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(storeDir, "cron_jobs.json"),
		MaxJobs:   5,
	})
	defer s.Stop()

	fi, err := os.Stat(storeDir)
	if err != nil {
		t.Fatalf("stat after NewScheduler: %v", err)
	}
	if !fi.IsDir() {
		t.Fatalf("storeDir is not a directory")
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("freshly-created storeDir perm = %#o, want 0o700", got)
	}
}
