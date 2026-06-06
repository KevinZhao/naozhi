package cron

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// newGetSessionArgs builds a getSessionArgs wired to a live inflight gate and
// stub finalizer for the executeGetSession branch tests. The router error is
// supplied via the scheduler's fakeRouter.getErr.
func newGetSessionArgs(t *testing.T, s *Scheduler, j *Job) getSessionArgs {
	t.Helper()
	inflight := s.jobInflight(j.ID)
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("initial CAS must succeed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return getSessionArgs{
		ctx:         ctx,
		spawnCancel: cancel,
		key:         "cron:" + j.ID,
		job:         j,
		snap:        jobSnapshot{jobID: j.ID, prompt: "ping"},
		runID:       "r-getsession",
		startedAt:   time.Now(),
		trigger:     TriggerScheduled,
		lg:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		finalizer:   &runFinalizer{inflight: inflight},
		inflight:    inflight,
	}
}

// TestExecuteGetSession_CanceledAbortsSkipPersist pins RNEW-003 (#423): when
// GetOrCreate returns context.Canceled, executeGetSession aborts with a
// skip-persist canceled finishRun (no IM notice) and signals abort=true.
func TestExecuteGetSession_CanceledAbortsSkipPersist(t *testing.T) {
	t.Parallel()
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{
		MaxJobs:   5,
		Router:    &fakeRouter{getErr: context.Canceled},
		Telemetry: rec,
	})
	j := &Job{ID: "job-getsession-cancel", Schedule: "@every 5m"}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	sess, _, abort := s.executeGetSession(newGetSessionArgs(t, s, j))
	if !abort {
		t.Fatal("canceled GetOrCreate must abort")
	}
	if sess != nil {
		t.Errorf("aborted spawn must return nil session, got %v", sess)
	}
	if rec.endedCount() != 1 {
		t.Fatalf("want 1 ended event, got %d", rec.endedCount())
	}
	got := rec.endedAtCron(0)
	if got.State != RunStateCanceled {
		t.Errorf("state: want canceled, got %q", got.State)
	}
	if got.ErrorClass != ErrClassCanceled {
		t.Errorf("error_class: want canceled, got %q", got.ErrorClass)
	}
}

// TestExecuteGetSession_SessionErrorAborts pins that a non-cancel GetOrCreate
// error classifies as a session error, drives finishRun (persisted), and
// signals abort=true.
func TestExecuteGetSession_SessionErrorAborts(t *testing.T) {
	t.Parallel()
	rec := &recordingBroadcaster{}
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		MaxJobs:   5,
		Router:    &fakeRouter{getErr: errors.New("spawn boom")},
		StorePath: dir + "/cron_jobs.json",
		Telemetry: rec,
	})
	j := &Job{ID: "job-getsession-err", Schedule: "@every 5m"}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	sess, _, abort := s.executeGetSession(newGetSessionArgs(t, s, j))
	if !abort {
		t.Fatal("session error must abort")
	}
	if sess != nil {
		t.Errorf("aborted spawn must return nil session, got %v", sess)
	}
	if rec.endedCount() != 1 {
		t.Fatalf("want 1 ended event, got %d", rec.endedCount())
	}
	got := rec.endedAtCron(0)
	if got.ErrorClass != ErrClassSessionError {
		t.Errorf("error_class: want session_error, got %q", got.ErrorClass)
	}
}
