package cron

// #1730 regression tests.
//
// resolveCronWorkspace's fresh=false branch used to consult only the TTL-cached
// workDirResolveUnderRootCached view when s.allowedRoot != "". An operator could
// point a WorkDir symlink at an allowed path, let the cache warm, then retarget
// it outside allowedRoot; within the cache TTL the next fresh=false tick would
// launch the CLI under the retargeted path. The fix adds an uncached
// workDirUnderRoot re-check after the cached gate passes, mirroring the
// fresh-path containment handling (RunStateFailed / ErrClassWorkDirOutsideRoot,
// no subprocess launch).
//
// These tests drive resolveCronWorkspace directly and assert on its
// (workDirForCLI, abort) return and the resulting job state.

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newResolveWorkspaceFixture(t *testing.T, allowedRoot string) *Scheduler {
	t.Helper()
	s := NewScheduler(SchedulerConfig{
		Router:      &fakeSessionRouter{},
		MaxJobs:     5,
		AllowedRoot: allowedRoot,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)
	return s
}

func runResolveWorkspace(s *Scheduler, j *Job, workDir string) (string, bool) {
	finalizer := &runFinalizer{inflight: s.jobInflight(j.ID)}
	snap := jobSnapshot{
		fresh:   false,
		workDir: workDir,
		jobID:   j.ID,
		prompt:  j.Prompt,
	}
	return s.resolveCronWorkspace(
		j, snap, "runid0000000001", time.Now(), TriggerScheduled, slog.Default(), finalizer)
}

func registerResolveJob(t *testing.T, s *Scheduler, j *Job) {
	t.Helper()
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()
}

// TestResolveWorkspace1730_RetargetAfterCacheWarmAborts is the core regression:
// the symlink resolves inside allowedRoot on the first call (warming the TTL
// cache as a positive), is then retargeted outside allowedRoot, and the next
// fresh=false call must abort (uncached re-check) rather than return a launch
// path — even though the cached gate would still say "ok".
func TestResolveWorkspace1730_RetargetAfterCacheWarmAborts(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inside := filepath.Join(root, "inside")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatalf("mkdir inside: %v", err)
	}
	outside := t.TempDir() // sibling tempdir, reachable but outside root

	link := filepath.Join(t.TempDir(), "workdir-link")
	if err := os.Symlink(inside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	s := newResolveWorkspaceFixture(t, root)
	j := &Job{ID: "job-retarget", Schedule: "@hourly", Prompt: "hi", Platform: "p", ChatID: "c", WorkDir: link}
	registerResolveJob(t, s, j)

	// First call: symlink points inside root -> resolves, warms the cache,
	// returns a launch path with abort=false.
	gotPath, abort := runResolveWorkspace(s, j, link)
	if abort {
		t.Fatalf("first call abort=true; want false for in-root symlink")
	}
	if !strings.HasPrefix(gotPath, root) {
		t.Fatalf("first call path = %q, want under root %q", gotPath, root)
	}

	// Retarget the symlink outside allowedRoot. The cached gate is still
	// positive within workDirResolveCacheTTL; the uncached re-check must catch
	// this and abort.
	if err := os.Remove(link); err != nil {
		t.Fatalf("remove link: %v", err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("re-symlink: %v", err)
	}

	gotPath, abort = runResolveWorkspace(s, j, link)
	if !abort {
		t.Fatalf("after retarget abort=false (path=%q); want true (uncached re-check must catch retarget)", gotPath)
	}
	if gotPath != "" {
		t.Errorf("after retarget path = %q, want empty on abort", gotPath)
	}

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

// TestResolveWorkspace1730_OutsideRootAborts covers the simpler shape: a
// workDir that is outside allowedRoot from the start must abort (cached gate
// itself rejects), no subprocess path returned.
func TestResolveWorkspace1730_OutsideRootAborts(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := t.TempDir()

	s := newResolveWorkspaceFixture(t, root)
	j := &Job{ID: "job-outside", Schedule: "@hourly", Prompt: "hi", Platform: "p", ChatID: "c", WorkDir: outside}
	registerResolveJob(t, s, j)

	gotPath, abort := runResolveWorkspace(s, j, outside)
	if !abort {
		t.Fatalf("abort=false (path=%q); want true for outside-root workDir", gotPath)
	}
	if gotPath != "" {
		t.Errorf("path = %q, want empty on abort", gotPath)
	}

	s.mu.RLock()
	gotClass := s.jobs[j.ID].LastErrorClass
	s.mu.RUnlock()
	if gotClass != ErrClassWorkDirOutsideRoot {
		t.Errorf("LastErrorClass = %q, want %q", gotClass, ErrClassWorkDirOutsideRoot)
	}
}

// TestResolveWorkspace1730_UnderRootResolves is the happy path: an in-root
// workDir must NOT abort and must return a launch path under root, so the
// uncached re-check does not over-fire and suppress legitimate runs.
func TestResolveWorkspace1730_UnderRootResolves(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inside := filepath.Join(root, "inside")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatalf("mkdir inside: %v", err)
	}

	s := newResolveWorkspaceFixture(t, root)
	j := &Job{ID: "job-inside", Schedule: "@hourly", Prompt: "hi", Platform: "p", ChatID: "c", WorkDir: inside}
	registerResolveJob(t, s, j)

	gotPath, abort := runResolveWorkspace(s, j, inside)
	if abort {
		t.Fatal("abort=true; want false for in-root workDir")
	}
	if !strings.HasPrefix(gotPath, root) {
		t.Errorf("path = %q, want under root %q", gotPath, root)
	}
}

// TestResolveWorkspace1730_NoAllowedRootSkipsCheck verifies the re-check is
// gated on allowedRoot being set: with sandbox disabled (allowedRoot==""),
// resolveCronWorkspace takes the best-effort EvalSymlinks path and never
// aborts, so no regression for the unconstrained case.
func TestResolveWorkspace1730_NoAllowedRootSkipsCheck(t *testing.T) {
	t.Parallel()
	s := newResolveWorkspaceFixture(t, "") // sandbox disabled
	workDir := t.TempDir()

	j := &Job{ID: "job-no-root", Schedule: "@hourly", Prompt: "hi", Platform: "p", ChatID: "c", WorkDir: workDir}
	registerResolveJob(t, s, j)

	gotPath, abort := runResolveWorkspace(s, j, workDir)
	if abort {
		t.Fatal("abort=true; want false when allowedRoot is unset")
	}
	if gotPath == "" {
		t.Error("path empty; want resolved workDir when allowedRoot is unset")
	}
}
