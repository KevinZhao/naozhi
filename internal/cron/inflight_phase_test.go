package cron

import (
	"context"
	"sync"
	"testing"
)

// phaseSampler records every distinct phase value it observes on a job's
// runInflight. Sampling points are wired into the run via the router
// (GetOrCreate = spawn phase entered) and the session (Send = send phase
// entered), so the recorded sequence reflects what a dashboard reader
// could observe at those moments — no sleep-based polling.
type phaseSampler struct {
	mu     sync.Mutex
	s      *Scheduler
	jobID  string
	phases []string
}

func (p *phaseSampler) sample() {
	view, ok := p.s.jobInflight(p.jobID).snapshot()
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if n := len(p.phases); n == 0 || p.phases[n-1] != view.Phase {
		p.phases = append(p.phases, view.Phase)
	}
}

func (p *phaseSampler) recorded() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.phases...)
}

// samplingRouter samples the inflight phase at GetOrCreate entry (the run is
// in PhaseSpawning by then — executeGetSession switches the badge before
// calling GetOrCreate) and hands back a sampling session.
type samplingRouter struct {
	sampler *phaseSampler
}

func (r samplingRouter) RegisterCronStubWithChain(key, workspace, lastPrompt string, chain []string) {
}
func (r samplingRouter) Reset(key string) {}
func (r samplingRouter) GetOrCreate(ctx context.Context, key string, opts AgentOpts) (Session, SessionStatus, error) {
	r.sampler.sample()
	return samplingSession{sampler: r.sampler}, SessionExisting, nil
}

// samplingSession samples the inflight phase at Send entry (PhaseSending by
// then — execSend switches the badge before sendWithWatchdog calls Send).
type samplingSession struct {
	sampler *phaseSampler
}

func (s samplingSession) Send(_ context.Context, _ string) (SendResult, error) {
	s.sampler.sample()
	return SendResult{Text: "ok", SessionID: "sess-phase-1"}, nil
}
func (s samplingSession) SessionID() string                     { return "sess-phase-1" }
func (s samplingSession) InterruptViaControl() InterruptOutcome { return InterruptUnsupported }

// TestExecuteOpt_PhaseSequence pins the H6 phase switch-point contract of RFC
// cron-sysession-merge §3.4 across the Phase C helper split: the dashboard
// "running 12s | spawning" badge reads runInflight.Phase, so every helper
// must keep its setPhase call. A full TriggerNow run must surface
// queued → spawning → sending in order (PhaseJittering is skipped by design:
// TriggerNow bypasses jitter, and the jitter path's badge is pinned separately
// by the applyJitterAndRecheck tests + jitter source anchors).
func TestExecuteOpt_PhaseSequence(t *testing.T) {
	t.Parallel()

	rec := &recordingBroadcaster{}
	sampler := &phaseSampler{jobID: "job-phase-seq"}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: samplingRouter{sampler: sampler}, Telemetry: rec})
	sampler.s = s

	j := &Job{ID: "job-phase-seq", Schedule: "@every 5m", Prompt: "ping"}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	// Sample once before the heavy phases: executeOpt has populated the view
	// with PhaseQueued by the time the run reaches GetOrCreate, but to also
	// pin the queued state itself we sample synchronously right after
	// TriggerNow-style invocation begins. Since executeOpt is synchronous in
	// this fixture (no jitter, fake router/session), instead sample from
	// inside the run via the router/session hooks plus one post-populate
	// check below.
	s.executeOpt(j, true /* viaTriggerNow: deterministic, skips jitter */)

	if rec.endedCount() != 1 {
		t.Fatalf("want 1 ended event, got %d", rec.endedCount())
	}
	if got := rec.endedAtCron(0); got.State != RunStateSucceeded {
		t.Fatalf("state: want succeeded, got %q (err=%q)", got.State, got.ErrorClass)
	}

	phases := sampler.recorded()
	want := []string{PhaseSpawning, PhaseSending}
	if len(phases) != len(want) {
		t.Fatalf("recorded phases = %v, want %v (H6 badge switch points dropped by a helper?)", phases, want)
	}
	for i := range want {
		if phases[i] != want[i] {
			t.Fatalf("phase[%d] = %q, want %q; full sequence %v (H6 ordering broken)", i, phases[i], want[i], phases)
		}
	}
}

// TestExecPopulateInflight_SeedsPhaseQueued pins the PhaseQueued switch point
// (H6) at its new Phase C home: execPopulateInflight's populate call must
// seed the inflight view with Phase=queued so the dashboard badge shows
// "queued" between CAS-win and the spawn/jitter transition.
func TestExecPopulateInflight_SeedsPhaseQueued(t *testing.T) {
	t.Parallel()

	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: okRouter{sid: "sess-q"}, Telemetry: rec})

	j := &Job{ID: "job-phase-queued", Schedule: "@every 5m", Prompt: "ping"}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	inflight := s.jobInflight(j.ID)
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("could not win the CAS for the test run")
	}
	defer inflight.running.Store(false)

	runID, _, _, ok := s.execPopulateInflight(j, true, inflight)
	if !ok {
		t.Fatal("execPopulateInflight aborted unexpectedly")
	}
	view, snapOK := inflight.snapshot()
	if !snapOK {
		t.Fatal("snapshot not visible after populate")
	}
	if view.Phase != PhaseQueued {
		t.Fatalf("Phase = %q, want %q (populate must seed the queued badge)", view.Phase, PhaseQueued)
	}
	if view.RunID != runID {
		t.Fatalf("RunID = %q, want %q", view.RunID, runID)
	}
}
