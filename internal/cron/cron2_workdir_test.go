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
	"strings"
	"testing"
)

func TestWorkDirReachable_EmptyPath(t *testing.T) {
	t.Parallel()
	if !workDirReachable("") {
		t.Error("workDirReachable(\"\") = false, want true (empty = router default)")
	}
}

func TestWorkDirReachable_ExistingDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if !workDirReachable(dir) {
		t.Errorf("workDirReachable(%q) = false, want true", dir)
	}
}

func TestWorkDirReachable_MissingDir(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if workDirReachable(missing) {
		t.Errorf("workDirReachable(%q) = true, want false (missing)", missing)
	}
}

func TestWorkDirReachable_FileNotDir(t *testing.T) {
	t.Parallel()
	f := filepath.Join(t.TempDir(), "plain.txt")
	if err := os.WriteFile(f, []byte("x"), 0600); err != nil {
		t.Fatalf("setup WriteFile: %v", err)
	}
	if workDirReachable(f) {
		t.Errorf("workDirReachable(%q) = true, want false (file, not dir)", f)
	}
}

// TestCRON2_FreshExecuteSkipsWhenWorkDirMissing is the integration test:
// with a fresh-mode job pointed at a deleted directory, execute() must
// NOT call Reset or GetOrCreate on the fake router, and MUST record a
// "work_dir unreachable" result on the job.
func TestCRON2_FreshExecuteSkipsWhenWorkDirMissing(t *testing.T) {
	fake := &fakeSessionRouter{}
	s := NewScheduler(SchedulerConfig{
		Router:  fake,
		MaxJobs: 5,
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

	s.execute(job)

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
	s.mu.RLock()
	stored := s.jobs[job.ID]
	var gotErr string
	if stored != nil {
		gotErr = stored.LastError
	}
	s.mu.RUnlock()
	if !strings.Contains(gotErr, "unreachable") {
		t.Errorf("LastError = %q, want contains %q", gotErr, "unreachable")
	}
}

// TestCRON2_FreshExecuteProceedsWhenWorkDirExists pairs with the skip
// test: if the guard over-fires we'd silently suppress legitimate runs.
// Here we give the job an existing tempdir and expect Reset + GetOrCreate
// to fire on the fake router as before.
func TestCRON2_FreshExecuteProceedsWhenWorkDirExists(t *testing.T) {
	fake := &fakeSessionRouter{}
	s := NewScheduler(SchedulerConfig{
		Router:  fake,
		MaxJobs: 5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	job := &Job{
		Schedule:     "@hourly",
		Prompt:       "hello",
		Platform:     "p",
		ChatID:       "c",
		WorkDir:      t.TempDir(),
		FreshContext: true,
	}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// execute triggers Reset + GetOrCreate under a valid workDir. fake
	// GetOrCreate returns (nil, 0, nil) so the dereference that follows
	// panics; absorb it — the assertions we care about are recorded
	// before the panic site.
	func() {
		defer func() { _ = recover() }()
		s.execute(job)
	}()

	fake.mu.Lock()
	defer fake.mu.Unlock()
	wantKey := "cron:" + job.ID
	resetFound := false
	for _, k := range fake.resetCalls {
		if k == wantKey {
			resetFound = true
			break
		}
	}
	if !resetFound {
		t.Errorf("Reset(%q) not seen after happy-path execute", wantKey)
	}
}

// TestCRON2_EmptyWorkDirPassesThrough covers the most common production
// shape: a job without an explicit WorkDir uses the router default.
// The guard must not reject this (workDirReachable("") returns true).
func TestCRON2_EmptyWorkDirPassesThrough(t *testing.T) {
	fake := &fakeSessionRouter{}
	s := NewScheduler(SchedulerConfig{
		Router:  fake,
		MaxJobs: 5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	job := &Job{
		Schedule:     "@hourly",
		Prompt:       "hello",
		Platform:     "p",
		ChatID:       "c",
		WorkDir:      "", // uses router default
		FreshContext: true,
	}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	func() {
		defer func() { _ = recover() }()
		s.execute(job)
	}()

	// Empty WorkDir must reach Reset (guard is permissive on empty).
	fake.mu.Lock()
	defer fake.mu.Unlock()
	wantKey := "cron:" + job.ID
	found := false
	for _, k := range fake.resetCalls {
		if k == wantKey {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Reset(%q) not seen with empty WorkDir", wantKey)
	}
}
