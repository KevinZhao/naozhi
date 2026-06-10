package cron

import (
	"context"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// reapRouter records Reset + RegisterCronStubWithChain calls so the
// fresh-context reap contract (#1829) can be asserted after a full
// executeOpt success run. GetOrCreate always returns a ready okSession.
type reapRouter struct {
	mu            sync.Mutex
	sid           string
	resetCalls    []string
	registerCalls []stubCall
}

func (r *reapRouter) RegisterCronStubWithChain(key, workspace, prompt string, chainIDs []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registerCalls = append(r.registerCalls, stubCall{key, workspace, prompt, chainIDs})
}

func (r *reapRouter) Reset(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resetCalls = append(r.resetCalls, key)
}

func (r *reapRouter) GetOrCreate(ctx context.Context, key string, opts AgentOpts) (Session, SessionStatus, error) {
	return okSession{id: r.sid}, SessionExisting, nil
}

func (r *reapRouter) snapshot() (resets []string, regs []stubCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.resetCalls...), append([]stubCall(nil), r.registerCalls...)
}

// deleteOnResetRouter deletes the job from the scheduler when the reap
// Reset (the 2nd Reset of a fresh success run: preflight is 1st) fires,
// reproducing a DeleteJobByID landing in the Reset→register window.
type deleteOnResetRouter struct {
	reapRouter
	s        *Scheduler
	jobID    string
	resetSeq int
}

func (r *deleteOnResetRouter) Reset(key string) {
	r.reapRouter.Reset(key)
	r.mu.Lock()
	r.resetSeq++
	seq := r.resetSeq
	r.mu.Unlock()
	if seq == 2 { // reap Reset — simulate concurrent Delete winning the race
		r.s.mu.Lock()
		delete(r.s.jobs, r.jobID)
		r.s.mu.Unlock()
	}
}

// TestFreshContextReapsSessionAfterSuccess pins #1829: a fresh_context cron
// job that finishes successfully MUST tear down its exempt session
// (Reset(cronKey)) and re-register a suspended sidebar stub chained to the
// run's session ID — so the idle CLI + MCP subprocess tree (~1.6 GB) is
// reclaimed immediately instead of squatting until the next tick. A
// regression that drops the post-success Reset reintroduces the exempt-
// session leak diagnosed 2026-06-06.
func TestFreshContextReapsSessionAfterSuccess(t *testing.T) {
	t.Parallel()

	rec := &recordingBroadcaster{}
	router := &reapRouter{sid: "sess-fresh-1"}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: router, Telemetry: rec})

	j := &Job{ID: "job-fresh-reap", Schedule: "@every 5m", Prompt: "ping", FreshContext: true}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	s.executeOpt(j, true /* viaTriggerNow: skip jitter */)

	if rec.endedCount() != 1 {
		t.Fatalf("want 1 ended event, got %d", rec.endedCount())
	}
	if got := rec.endedAtCron(0); got.State != RunStateSucceeded {
		t.Fatalf("state: want succeeded, got %q (err=%q)", got.State, got.ErrorClass)
	}

	wantKey := sessionkey.CronKey(j.ID)
	resets, regs := router.snapshot()

	// Two Reset(cronKey) calls happen on a fresh success run:
	//   1. preflight Reset (run start) — destroys any prior session.
	//   2. post-success reap Reset (#1829) — releases the session we just used.
	resetCount := 0
	for _, k := range resets {
		if k == wantKey {
			resetCount++
		}
	}
	if resetCount < 2 {
		t.Errorf("Reset(%q) count = %d, want >=2 (preflight + post-success reap); resets=%v",
			wantKey, resetCount, resets)
	}

	// The reap must re-register a suspended stub chained to the run's
	// session ID so the sidebar row + JSONL history survive the idle gap.
	var reapStub *stubCall
	for i := len(regs) - 1; i >= 0; i-- {
		if regs[i].key == wantKey {
			reapStub = &regs[i]
			break
		}
	}
	if reapStub == nil {
		t.Fatalf("no stub re-registered for %q after reap; regs=%v", wantKey, regs)
	}
	if len(reapStub.chainIDs) != 1 || reapStub.chainIDs[0] != "sess-fresh-1" {
		t.Errorf("reap stub chainIDs = %v, want [sess-fresh-1] so prior-run history stays clickable",
			reapStub.chainIDs)
	}
}

// TestFreshReapSkipsStubReregisterWhenJobDeleted pins the zombie-stub guard
// in #1829: if the job is deleted between the post-success Reset and the stub
// re-register (DeleteJobByID's teardown does NOT take the inflight CAS gate,
// so it races the success tail), the reap must NOT resurrect a sidebar stub
// for the now-deleted job. Simulated by removing the job from s.jobs before
// executeOpt reaches its success-tail re-check — the existence re-check then
// observes the job gone and skips registerStubByValue.
func TestFreshReapSkipsStubReregisterWhenJobDeleted(t *testing.T) {
	t.Parallel()

	rec := &recordingBroadcaster{}
	// deleteOnResetRouter deletes the job from the scheduler the moment the
	// post-success Reset(cronKey) fires, reproducing a concurrent Delete that
	// lands in the Reset→register window.
	router := &deleteOnResetRouter{reapRouter: reapRouter{sid: "sess-del-1"}}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: router, Telemetry: rec})
	router.s = s

	j := &Job{ID: "job-deleted-mid", Schedule: "@every 5m", Prompt: "ping", FreshContext: true}
	router.jobID = j.ID
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	s.executeOpt(j, true)

	wantKey := sessionkey.CronKey(j.ID)
	_, regs := router.snapshot()
	// The preflight register (run start) is fine, but the POST-success reap
	// register must be skipped. The router deletes the job on the SECOND
	// Reset (the reap Reset), so any register recorded after that point is a
	// zombie. We assert no register carries the reap's session ID — the
	// preflight register chains the prior LastSessionID (empty here), never
	// "sess-del-1".
	for _, r := range regs {
		if r.key == wantKey && len(r.chainIDs) == 1 && r.chainIDs[0] == "sess-del-1" {
			t.Errorf("reap re-registered a zombie stub for deleted job: %+v", r)
		}
	}
}

// TestFreshReapEmptySessionIDRegistersChainlessStub pins #1845: when a
// fresh-context cron run succeeds but the result carries an empty session ID
// (anomalous — process.go normally fills it on the success frame), the reap
// must still complete cleanly: the run is Succeeded, the stub is re-registered
// exactly once with NO chain (no clickable history), and nothing panics. The
// empty-ID branch only adds a diagnostic Warn; it must not change the
// registerStubByValue call or the run state.
func TestFreshReapEmptySessionIDRegistersChainlessStub(t *testing.T) {
	t.Parallel()

	rec := &recordingBroadcaster{}
	// Empty sid → okSession.Send returns SendResult{SessionID: ""}, so the
	// success-tail reap sees result.SessionID == "".
	router := &reapRouter{sid: ""}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: router, Telemetry: rec})

	j := &Job{ID: "job-empty-sid", Schedule: "@every 5m", Prompt: "ping", FreshContext: true}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	s.executeOpt(j, true /* viaTriggerNow: skip jitter */)

	if rec.endedCount() != 1 {
		t.Fatalf("want 1 ended event, got %d", rec.endedCount())
	}
	if got := rec.endedAtCron(0); got.State != RunStateSucceeded {
		t.Fatalf("state: want succeeded, got %q (err=%q)", got.State, got.ErrorClass)
	}

	wantKey := sessionkey.CronKey(j.ID)
	_, regs := router.snapshot()

	// Exactly one reap stub for this job, and it must be chain-less because
	// the session ID was empty.
	var reapStub *stubCall
	reapStubCount := 0
	for i := range regs {
		if regs[i].key == wantKey {
			reapStubCount++
			reapStub = &regs[i]
		}
	}
	if reapStubCount != 1 {
		t.Fatalf("want exactly 1 stub re-register for %q, got %d; regs=%v", wantKey, reapStubCount, regs)
	}
	if len(reapStub.chainIDs) != 0 {
		t.Errorf("empty session ID must yield a chain-less stub; got chainIDs=%v", reapStub.chainIDs)
	}
}

// TestPersistentContextNotReapedAfterSuccess pins the negative half of #1829:
// a persistent-mode (FreshContext=false) cron job must NOT be torn down after
// a successful run — its session is deliberately reused across ticks to carry
// conversational context. A regression that reaps persistent sessions would
// destroy that context every tick.
func TestPersistentContextNotReapedAfterSuccess(t *testing.T) {
	t.Parallel()

	rec := &recordingBroadcaster{}
	router := &reapRouter{sid: "sess-persist-1"}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: router, Telemetry: rec})

	j := &Job{ID: "job-persist", Schedule: "@every 5m", Prompt: "ping", FreshContext: false}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	s.executeOpt(j, true)

	if got := rec.endedAtCron(0); got.State != RunStateSucceeded {
		t.Fatalf("state: want succeeded, got %q (err=%q)", got.State, got.ErrorClass)
	}

	wantKey := sessionkey.CronKey(j.ID)
	resets, _ := router.snapshot()
	for _, k := range resets {
		if k == wantKey {
			t.Errorf("persistent-mode job must not be Reset after success; got Reset(%q) in %v",
				wantKey, resets)
		}
	}
}
