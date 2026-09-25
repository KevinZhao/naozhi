package cron

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// TestStart_ClaimsAdoptableRunBeforeFirstTick is #2751's ordering invariant,
// asserted through Start() — every other adoption test calls the reconciler
// directly, so the order relative to s.cron.Start() was never exercised.
//
// When Start returns, an adoptable run must ALREADY hold its job's gate. If the
// claim ran after the cron loop started (it used to run in a startup
// goroutine), a tick due at that moment could win the CAS first: adoption then
// falls back to recording the run interrupted while its CLI is still alive and
// mid-turn, and the tick sends a second turn into that same session. With the
// gate held first, the tick overlap-skips instead.
//
// The check is the gate's own state rather than a raced real tick, so it is
// deterministic: no schedule has to fire inside a millisecond window.
func TestStart_ClaimsAdoptableRunBeforeFirstTick(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "cron_jobs.json")

	// Process A: a persisted job whose run wrote its marker and never finished.
	a := NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: storePath}, SchedulerDeps{Router: &fakeRouter{}})
	if err := a.Start(); err != nil {
		t.Fatalf("A Start: %v", err)
	}
	j := &Job{Schedule: "@every 5m", Prompt: "do thing", Platform: "feishu", ChatID: "c1", ChatType: "direct"}
	if err := a.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	runID := mustGenerateRunID()
	if path := a.writeRunInflightMarker(runInflightMarker{
		JobID: j.ID, RunID: runID, Trigger: TriggerScheduled,
		StartedAtMS: time.Now().Add(-90 * time.Second).UnixMilli(),
		Prompt:      "do thing",
	}, slog.Default()); path == "" {
		t.Fatal("marker write failed")
	}
	a.gcWG.Wait()
	a.Stop()

	// Process B: its router still holds the CLI mid-turn.
	key := "cron:" + j.ID
	live := &fakeInFlightRun{ready: make(chan struct{})}
	t.Cleanup(func() { close(live.ready) })
	router := &adoptingRouter{
		verdicts: map[string]AdoptVerdict{key: AdoptLive},
		runs:     map[string]*fakeInFlightRun{key: live},
	}
	b := NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: storePath}, SchedulerDeps{Router: router})
	if err := b.Start(); err != nil {
		t.Fatalf("B Start: %v", err)
	}
	t.Cleanup(b.Stop)

	// No waiting of any kind before this read: the claim has to have happened
	// inside Start, not in something Start merely launched.
	inf, ok := b.gate.peek(j.ID)
	if !ok || !inf.running.Load() {
		t.Fatal("Start returned with the adoptable run's gate free — the first tick could win the CAS and double-run the live CLI")
	}
	if v, ok := inf.snapshot(); !ok || v.RunID != runID {
		t.Errorf("gate holds run %+v (ok=%v), want the adopted run %q", v, ok, runID)
	}
}
