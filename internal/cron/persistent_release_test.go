package cron

import (
	"errors"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// releaseRouter is errSendRouter plus the ProcessReleaser capability: each
// ReleaseProcess is recorded as "release" in the shared order.
type releaseRouter struct {
	errSendRouter
	relMu    sync.Mutex
	released []string
}

func (r *releaseRouter) ReleaseProcess(key string) bool {
	r.order.record("release")
	r.relMu.Lock()
	defer r.relMu.Unlock()
	r.released = append(r.released, key)
	return true
}

func (r *releaseRouter) releasedKeys() []string {
	r.relMu.Lock()
	defer r.relMu.Unlock()
	return append([]string(nil), r.released...)
}

var _ ProcessReleaser = (*releaseRouter)(nil)

// runReleaseCase runs one TriggerNow execution of a job against a
// releaseRouter whose session is sess.
func runReleaseCase(t *testing.T, sess Session, fresh bool) (*releaseRouter, *orderRecorder, *recordingBroadcaster, string) {
	t.Helper()
	ord := &orderRecorder{}
	rec := &recordingBroadcaster{order: ord}
	router := &releaseRouter{errSendRouter: errSendRouter{reapRouter: reapRouter{order: ord}, sess: sess}}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: router, Telemetry: rec})
	j := &Job{ID: "job-release", Schedule: "@every 5m", Prompt: "ping", FreshContext: fresh}
	s.putJobForTest(j)
	s.executeOpt(j.ID, true /* viaTriggerNow: skip jitter */)
	if rec.endedCount() != 1 {
		t.Fatalf("want 1 ended event, got %d", rec.endedCount())
	}
	return router, ord, rec, sessionkey.CronKey(j.ID)
}

// A persistent-context run releases its process once, before the finishRun
// that releases the CAS gate (run-ended is emitted by that same finishRun): a
// later release could close the process a concurrent TriggerNow just fetched.
// The session itself is kept — no Reset.
func TestPersistentRun_ReleasesProcessWhileGateHeld(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		sess      Session
		wantState RunState
	}{
		{"success", okSession{id: "sess-keep"}, RunStateSucceeded},
		{"send_error", errSendSession{sendErr: errors.New("send failed: connection reset")}, RunStateFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			router, ord, rec, key := runReleaseCase(t, tc.sess, false)
			if got := rec.endedAtCron(0).State; got != tc.wantState {
				t.Fatalf("state = %q, want %q", got, tc.wantState)
			}
			if got := router.releasedKeys(); len(got) != 1 || got[0] != key {
				t.Fatalf("ReleaseProcess calls = %v, want exactly [%s]", got, key)
			}
			ord.assertCountBefore(t, "release", 1, "run-ended",
				"the persistent release must land while the CAS gate is held")
			if resets, _ := router.snapshot(); len(resets) != 0 {
				t.Errorf("persistent run must keep its session; got Reset calls %v", resets)
			}
		})
	}
}

// A canceled run keeps its process: on shutdown the CLI is still mid-turn
// behind its shim and the next process adopts the run (#2712); releasing it
// would turn every restart-spanning run into an interrupted one.
func TestPersistentRun_CancelKeepsProcess(t *testing.T) {
	t.Parallel()
	router, _, rec, _ := runReleaseCase(t, cancelSendSession{}, false)
	if got := rec.endedAtCron(0).State; got != RunStateCanceled {
		t.Fatalf("state = %q, want canceled", got)
	}
	if got := router.releasedKeys(); len(got) != 0 {
		t.Errorf("canceled run released its process: %v", got)
	}
}

// A fresh-context run never asks for a release: its Reset already closes the
// process and drops the session, and a release on top would be a second
// teardown of a key the reap re-registered as a stub.
func TestFreshRun_DoesNotRelease(t *testing.T) {
	t.Parallel()
	for name, sess := range map[string]Session{
		"success":    okSession{id: "sess-fresh"},
		"send_error": errSendSession{sendErr: errors.New("boom")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			router, _, _, _ := runReleaseCase(t, sess, true)
			if got := router.releasedKeys(); len(got) != 0 {
				t.Errorf("fresh run called ReleaseProcess: %v", got)
			}
		})
	}
}
