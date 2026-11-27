package cron

// CRON2 regression tests. Before Round 100 the fresh-mode execute branch
// called router.Reset(key) before anything checked whether workDir still
// existed; an admin who removed the workspace on disk would therefore
// destroy the previous session's history and then have spawnSession
// fail when shim StartShim attempted cwd=<missing> — the worst of both
// worlds. The new workDirReachable gate runs before Reset so a missing
// workspace just skips the run (records the error, delivers a notice)
// and preserves the prior session so it can pick up when the workspace
// is restored.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// TestCRON2_FreshExecuteSkipsWhenWorkDirMissing is the integration test:
// with a fresh-mode job pointed at a deleted directory, execute() must
// NOT call Reset or GetOrCreate on the fake router, and MUST record a
// "work_dir unreachable" result on the job.
func TestCRON2_FreshExecuteSkipsWhenWorkDirMissing(t *testing.T) {
	t.Parallel()
	fake := &fakeSessionRouter{}
	s := NewScheduler(SchedulerConfig{
		MaxJobs: 5,
	}, SchedulerDeps{
		Router: fake,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	// Build workDir then delete it — mimics an admin removing a workspace
	// between job creation and its next tick.
	workDir := t.TempDir()
	gone := filepath.Join(workDir, "gone")
	if err := os.Mkdir(gone, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	job := &Job{
		Schedule:     "@hourly",
		Prompt:       "hello",
		Platform:     "p",
		ChatID:       "c",
		WorkDir:      gone,
		FreshContext: true,
	}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Baseline: AddJob itself triggered 1 RegisterCronStub, 0 Reset,
	// 0 GetOrCreate.
	fake.mu.Lock()
	baselineReset := len(fake.resetCalls)
	baselineGetCreate := len(fake.getCreateKeys)
	fake.mu.Unlock()

	s.executeOpt(job.ID, false)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := len(fake.resetCalls) - baselineReset; got != 0 {
		t.Errorf("Reset calls after missing-workdir execute = %d, want 0", got)
	}
	if got := len(fake.getCreateKeys) - baselineGetCreate; got != 0 {
		t.Errorf("GetOrCreate calls after missing-workdir execute = %d, want 0", got)
	}

	// The job should have LastError set to the unreachable reason so the
	// dashboard can surface it — mirrors the allowed_root rejection path.
	var gotErr string
	if stored, ok := s.tbl.snapshot(job.ID); ok {
		gotErr = stored.LastError
	}
	if !strings.Contains(gotErr, "unreachable") {
		t.Errorf("LastError = %q, want contains %q", gotErr, "unreachable")
	}
}

func TestWorkDirReachable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		"":                                   true, // router default
		dir:                                  true,
		filepath.Join(dir, "does-not-exist"): false,
		file:                                 false, // not a directory
	} {
		if got := workDirReachable(path); got != want {
			t.Errorf("workDirReachable(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestCRON2_FreshExecuteProceeds: the guard must not over-fire — an existing
// workDir, or none (the router default), still reaches Reset.
func TestCRON2_FreshExecuteProceeds(t *testing.T) {
	t.Parallel()
	for name, workDir := range map[string]string{"existing workdir": t.TempDir(), "empty workdir": ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeSessionRouter{}
			s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: fake})
			if err := s.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer s.Stop()
			job := &Job{Schedule: "@hourly", Prompt: "hello", Platform: "p", ChatID: "c", WorkDir: workDir, FreshContext: true}
			if err := s.AddJob(job); err != nil {
				t.Fatalf("AddJob: %v", err)
			}
			// fakeSessionRouter's GetOrCreate returns a nil session, so the run
			// panics after Reset; the Reset is what this test is after.
			func() {
				defer func() { _ = recover() }()
				s.executeOpt(job.ID, false)
			}()
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if !slices.Contains(fake.resetCalls, sessionkey.CronKey(job.ID)) {
				t.Errorf("Reset(%q) not seen", sessionkey.CronKey(job.ID))
			}
		})
	}
}
