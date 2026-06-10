package cron

// R20260603140013-CR-3 regression tests.
//
// freshContextPreflightP0 calls s.router.Reset(key) — which destroys the
// cron:<jobID> session, its CLI process, and its history — before the run
// proceeds. resolveCronWorkspace (in executeOpt) already aborts outside-root
// runs, but it consults the TTL-cached workDirResolveUnderRootCached view; a
// symlink retargeted outside allowedRoot within that TTL would pass there as a
// stale-positive and let the run reach this preflight, blowing away a live
// session for a run that can never succeed. The fix adds an uncached
// workDirUnderRoot early-check BEFORE the Reset so an outside-root workDir
// fails the run WITHOUT tearing down the existing session.
//
// These tests drive freshContextPreflightP0 directly (bypassing the upstream
// resolveCronWorkspace gate) so the new branch is exercised in isolation.

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

func newFreshPreflightFixture(t *testing.T, allowedRoot string) (*Scheduler, *fakeSessionRouter) {
	t.Helper()
	fake := &fakeSessionRouter{}
	s := NewScheduler(SchedulerConfig{
		MaxJobs:     5,
		AllowedRoot: allowedRoot,
	}, SchedulerDeps{
		Router: fake,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)
	return s, fake
}

func runFreshPreflight(s *Scheduler, j *Job, workDir string) (stubRefresher, bool) {
	key := sessionkey.CronKey(j.ID)
	finalizer := &runFinalizer{inflight: s.jobInflight(j.ID)}
	snap := jobSnapshot{
		fresh:   true,
		workDir: workDir,
		jobID:   j.ID,
		prompt:  j.Prompt,
	}
	return s.freshContextPreflightP0(preflightArgs{
		job:       j,
		snap:      snap,
		key:       key,
		lg:        slog.Default(),
		runID:     "runid0000000001",
		startedAt: time.Now(),
		trigger:   TriggerScheduled,
		finalizer: finalizer,
	})
}

// TestCRON3_FreshPreflightSkipsWhenWorkDirOutsideRoot is the core regression:
// a fresh-mode job whose (reachable) workDir is outside allowedRoot must NOT
// reach s.router.Reset; the run is failed with the outside-root error class
// and the existing session is preserved.
func TestCRON3_FreshPreflightSkipsWhenWorkDirOutsideRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := t.TempDir() // sibling tempdir, reachable but outside root
	s, fake := newFreshPreflightFixture(t, root)

	j := &Job{
		ID:           "job-outside-root",
		Schedule:     "@hourly",
		Prompt:       "hi",
		Platform:     "p",
		ChatID:       "c",
		WorkDir:      outside,
		FreshContext: true,
	}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	_, ok := runFreshPreflight(s, j, outside)
	if ok {
		t.Fatal("preflight ok=true; want false for outside-root workDir")
	}

	fake.mu.Lock()
	for _, k := range fake.resetCalls {
		if k == sessionkey.CronKey(j.ID) {
			fake.mu.Unlock()
			t.Fatalf("Reset(%q) called for outside-root workDir; existing session must be preserved", k)
		}
	}
	fake.mu.Unlock()

	s.mu.RLock()
	gotErr := s.jobs[j.ID].LastError
	gotClass := s.jobs[j.ID].LastErrorClass
	s.mu.RUnlock()
	if !strings.Contains(gotErr, "outside allowed root") {
		t.Errorf("LastError = %q, want contains %q", gotErr, "outside allowed root")
	}
	if gotClass != ErrClassWorkDirOutsideRoot {
		t.Errorf("LastErrorClass = %q, want %q", gotClass, ErrClassWorkDirOutsideRoot)
	}
}

// TestCRON3_FreshPreflightProceedsWhenWorkDirUnderRoot pairs with the skip
// test: an in-root workDir must reach Reset so the guard does not over-fire
// and suppress legitimate fresh runs.
func TestCRON3_FreshPreflightProceedsWhenWorkDirUnderRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s, fake := newFreshPreflightFixture(t, root)

	j := &Job{
		ID:           "job-inside-root",
		Schedule:     "@hourly",
		Prompt:       "hi",
		Platform:     "p",
		ChatID:       "c",
		WorkDir:      root,
		FreshContext: true,
	}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	_, ok := runFreshPreflight(s, j, root)
	if !ok {
		t.Fatal("preflight ok=false; want true for in-root reachable workDir")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	found := false
	for _, k := range fake.resetCalls {
		if k == sessionkey.CronKey(j.ID) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Reset(%q) not seen for in-root workDir", sessionkey.CronKey(j.ID))
	}
}

// TestCRON3_FreshPreflightNoAllowedRootSkipsCheck verifies the guard is gated
// on allowedRoot being set: with sandbox disabled (allowedRoot==""), an
// arbitrary workDir must still reach Reset (the containment check does not
// apply).
func TestCRON3_FreshPreflightNoAllowedRootSkipsCheck(t *testing.T) {
	t.Parallel()
	s, fake := newFreshPreflightFixture(t, "") // sandbox disabled
	workDir := t.TempDir()

	j := &Job{
		ID:           "job-no-root",
		Schedule:     "@hourly",
		Prompt:       "hi",
		Platform:     "p",
		ChatID:       "c",
		WorkDir:      workDir,
		FreshContext: true,
	}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	_, ok := runFreshPreflight(s, j, workDir)
	if !ok {
		t.Fatal("preflight ok=false; want true when allowedRoot is unset")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	found := false
	for _, k := range fake.resetCalls {
		if k == sessionkey.CronKey(j.ID) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Reset(%q) not seen with allowedRoot unset", sessionkey.CronKey(j.ID))
	}
}
