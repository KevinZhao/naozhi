package cron

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedStore writes jobs to a fresh store through one scheduler's lifecycle and
// returns its path.
func seedStore(t *testing.T, jobs ...*Job) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cron.json")
	s := NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: 10}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("seed Start: %v", err)
	}
	for _, j := range jobs {
		if err := s.AddJob(j); err != nil {
			t.Fatalf("seed AddJob: %v", err)
		}
	}
	s.Stop()
	return path
}

func startOn(t *testing.T, cfg SchedulerConfig) *Scheduler {
	t.Helper()
	s := NewScheduler(cfg, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)
	return s
}

// Start registers a cron entry for every active job it loads, and none for a
// paused one.
func TestStart_CommitsEntriesForLoadedActiveJobs(t *testing.T) {
	active := &Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "c", ChatType: "direct"}
	paused := &Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "c", ChatType: "direct", Paused: true}
	path := seedStore(t, active, paused)

	s := startOn(t, SchedulerConfig{StorePath: path, MaxJobs: 10})
	if n := len(s.cron.Entries()); n != 1 {
		t.Errorf("robfig entries = %d, want 1 (the active job)", n)
	}
	if s.NextRun(&Job{ID: active.ID}).IsZero() {
		t.Error("loaded active job has no next run")
	}
	if !s.NextRun(&Job{ID: paused.ID}).IsZero() {
		t.Error("loaded paused job has a next run")
	}
}

// A persisted job whose work_dir escapes AllowedRoot is not loaded.
func TestStart_SkipsWorkDirOutsideAllowedRoot(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "proj")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	in := &Job{Schedule: "@every 1h", Prompt: "p", WorkDir: inside, Platform: "feishu", ChatID: "c", ChatType: "direct", Paused: true}
	out := &Job{Schedule: "@every 1h", Prompt: "p", WorkDir: t.TempDir(), Platform: "feishu", ChatID: "c", ChatType: "direct", Paused: true}
	path := seedStore(t, in, out)

	s := startOn(t, SchedulerConfig{StorePath: path, MaxJobs: 10, AllowedRoot: root})
	if _, ok := s.tbl.snapshot(in.ID); !ok {
		t.Error("job inside AllowedRoot was not loaded")
	}
	if _, ok := s.tbl.snapshot(out.ID); ok {
		t.Error("job outside AllowedRoot was loaded")
	}
}

// A terminal result carrying a new session id reaches KnownSessionIDs at once,
// not after the cache TTL.
func TestRecordTerminalResult_NewSessionInvalidatesKnownSessions(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	if _, ok := s.KnownSessionIDs()["fresh-session"]; ok {
		t.Fatal("precondition: session already known")
	}
	if _, _, ok := s.recordTerminalResult(id, "done", "", "fresh-session", "", RunStateSucceeded, time.Now()); !ok {
		t.Fatal("recordTerminalResult failed")
	}
	if _, ok := s.KnownSessionIDs()["fresh-session"]; !ok {
		t.Error("a new session id is not in KnownSessionIDs until the cache expires")
	}
}

// Stop writes the job set even when the file is gone from under it.
func TestStop_PersistsJobs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron.json")
	s := NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: 10}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	j := &Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "c", ChatType: "direct", Paused: true}
	if err := s.AddJob(j); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	s.Stop()
	if jobs, err := loadJobs(path); err != nil || jobs[j.ID] == nil {
		t.Errorf("after Stop: %v, job present=%v", err, jobs[j.ID] != nil)
	}
}

// A shutdown save that does not land is reported: there is no later save to
// retry it (#690, #1301). Not parallel: slog.SetDefault is process-global.
func TestStop_ReportsShutdownSaveThatDidNotLand(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{StorePath: filepath.Join(dir, "cron.json"), MaxJobs: 10}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.AddJob(&Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "c", ChatType: "direct", Paused: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	read, restore := withCapturedSlog(t)
	s.Stop()
	restore()
	if !strings.Contains(read(), "save cron store on shutdown failed") {
		t.Errorf("no shutdown-save failure logged; got:\n%s", read())
	}
}

// TriggerNow refuses a job that cannot run, and runs one that can.
func TestTriggerNow_RefusesOrRuns(t *testing.T) {
	r := &reapRouter{sid: "s1"}
	s := NewScheduler(SchedulerConfig{StorePath: filepath.Join(t.TempDir(), "cron.json"), MaxJobs: 10}, SchedulerDeps{Router: r})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	add := func(j *Job) string {
		t.Helper()
		j.Platform, j.ChatID, j.ChatType = "feishu", "c", "direct"
		if err := s.AddJob(j); err != nil {
			t.Fatal(err)
		}
		return j.ID
	}
	runnable := add(&Job{Schedule: "@every 1h", Prompt: "p"})
	paused := add(&Job{Schedule: "@every 1h", Prompt: "p", Paused: true})
	noPrompt := add(&Job{Schedule: "@every 1h", Paused: true})
	for _, tc := range []struct {
		id   string
		want error
	}{{"nope", ErrJobNotFound}, {paused, ErrJobPaused}} {
		if err := s.TriggerNow(tc.id); !errors.Is(err, tc.want) {
			t.Errorf("TriggerNow(%s) = %v, want %v", tc.id, err, tc.want)
		}
	}
	s.editJobForTest(t, noPrompt, func(j *Job) { j.Paused = false })
	if err := s.TriggerNow(noPrompt); !errors.Is(err, ErrJobNoPrompt) {
		t.Errorf("TriggerNow(no prompt) = %v, want ErrJobNoPrompt", err)
	}

	if err := s.TriggerNow(runnable); err != nil {
		t.Fatal(err)
	}
	s.triggerWG.Wait()
	if j, _ := s.tbl.snapshot(runnable); j.RunCounters.Total != 1 || j.LastRunAt.IsZero() {
		t.Errorf("after TriggerNow: counters %+v, last run %v; want one recorded run", j.RunCounters, j.LastRunAt)
	}
}
