package cron

import (
	"log/slog"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// adoptReapRouter records the session cleanup an adoption performs: Reset and
// stub registration through reapRouter, ReleaseProcess here, all into one
// shared order with the run-ended event. Each cleanup call also probes the
// job's gate, so a test can assert it ran before finishRun released it.
type adoptReapRouter struct {
	reapRouter
	run      *fakeInFlightRun
	gateHeld func() bool

	relMu      sync.Mutex
	released   []string
	gateFreeAt []string // cleanup calls that found the gate already released
}

func (r *adoptReapRouter) AdoptInFlight(key string) (InFlightRun, AdoptVerdict) {
	return r.run, AdoptLive
}

func (r *adoptReapRouter) probeGate(event string) {
	if r.gateHeld() {
		return
	}
	r.relMu.Lock()
	defer r.relMu.Unlock()
	r.gateFreeAt = append(r.gateFreeAt, event)
}

func (r *adoptReapRouter) Reset(key string) {
	r.probeGate("reset")
	r.reapRouter.Reset(key)
}

func (r *adoptReapRouter) RegisterCronStubWithChain(key, workspace, prompt string, chainIDs []string) {
	r.probeGate("register-stub")
	r.reapRouter.RegisterCronStubWithChain(key, workspace, prompt, chainIDs)
}

func (r *adoptReapRouter) ReleaseProcess(key string) bool {
	r.probeGate("release")
	r.order.record("release")
	r.relMu.Lock()
	defer r.relMu.Unlock()
	r.released = append(r.released, key)
	return true
}

// assertGateHeld fails if any cleanup call ran after the job's gate was freed.
func (r *adoptReapRouter) assertGateHeld(t *testing.T) {
	t.Helper()
	r.relMu.Lock()
	defer r.relMu.Unlock()
	if len(r.gateFreeAt) != 0 {
		t.Errorf("cleanup ran after the gate was released: %v", r.gateFreeAt)
	}
}

func (r *adoptReapRouter) releasedKeys() []string {
	r.relMu.Lock()
	defer r.relMu.Unlock()
	return append([]string(nil), r.released...)
}

var (
	_ InFlightAdopter = (*adoptReapRouter)(nil)
	_ ProcessReleaser = (*adoptReapRouter)(nil)
)

type adoptReapCase struct {
	s      *Scheduler
	router *adoptReapRouter
	rec    *recordingBroadcaster
	ord    *orderRecorder
	jobID  string
	key    string
}

// startAdoption seeds a job whose previous process left a marker in the given
// mode, and starts adopting it; the adopted turn resolves with outcome once
// the test closes c.router.run.ready.
func startAdoption(t *testing.T, fresh bool, outcome AdoptedRunOutcome) adoptReapCase {
	t.Helper()
	ord := &orderRecorder{}
	rec := &recordingBroadcaster{order: ord}
	router := &adoptReapRouter{
		reapRouter: reapRouter{order: ord},
		run:        &fakeInFlightRun{outcome: outcome, ready: make(chan struct{})},
	}
	storePath := filepath.Join(t.TempDir(), "cron_jobs.json")
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: storePath},
		SchedulerDeps{Router: router, Telemetry: rec})
	jobID := mustGenerateID()
	router.gateHeld = func() bool { _, ok := s.CurrentRun(jobID); return ok }
	s.putJobForTest(&Job{ID: jobID, Schedule: "@every 5m", Prompt: "do thing",
		WorkDir: "/tmp/wd", FreshContext: fresh, LastSessionID: "sess-prev"})
	if path := s.writeRunInflightMarker(runInflightMarker{
		JobID: jobID, RunID: mustGenerateRunID(), Trigger: TriggerScheduled,
		StartedAtMS: time.Now().Add(-90 * time.Second).UnixMilli(),
		Prompt:      "do thing", WorkDir: "/tmp/wd", Fresh: fresh,
	}, slog.Default()); path == "" {
		t.Fatal("marker write failed")
	}
	s.reconcileRunInflight()
	return adoptReapCase{s: s, router: router, rec: rec, ord: ord, jobID: jobID, key: sessionkey.CronKey(jobID)}
}

// settle resolves the adopted turn and waits for the adoption to finish.
func (c adoptReapCase) settle(t *testing.T) {
	t.Helper()
	close(c.router.run.ready)
	c.s.gcWG.Wait()
	if n := c.rec.endedCount(); n != 1 {
		t.Fatalf("want 1 run-ended event from the adoption, got %d", n)
	}
}

// A fresh-context run adopted across a restart is reaped like a local one: its
// CLI is Reset and the sidebar stub re-registered with a chain to the run's
// session, both while the adoption still holds the job's gate. Without the
// reap the exempt session stays resident until the job's next tick (#3103).
func TestAdoption_FreshSuccessReapsSessionWhileGateHeld(t *testing.T) {
	t.Parallel()
	c := startAdoption(t, true, AdoptedRunOutcome{Completed: true, Text: "ok", SessionID: "sess-a1"})
	c.settle(t)

	resets, regs := c.router.snapshot()
	if !reflect.DeepEqual(resets, []string{c.key}) {
		t.Fatalf("Reset calls = %v, want exactly [%s]", resets, c.key)
	}
	if len(regs) != 1 || regs[0].key != c.key || regs[0].workspace != "/tmp/wd" ||
		regs[0].prompt != "do thing" || !reflect.DeepEqual(regs[0].chainIDs, []string{"sess-a1"}) {
		t.Fatalf("stub registrations = %+v, want one on %s in /tmp/wd for \"do thing\" chained to [sess-a1]", regs, c.key)
	}
	c.router.assertGateHeld(t)
	c.ord.assertCountBefore(t, "reset", 1, "run-ended", "the adopted fresh session must be reset while the gate is held")
	c.ord.assertCountBefore(t, "register-stub", 1, "run-ended", "the stub must be re-registered while the gate is held")
	if got := c.router.releasedKeys(); len(got) != 0 {
		t.Errorf("fresh run must not take the persistent release; got %v", got)
	}
}

// Every other non-shutdown ending of a fresh adopted run resets too, mirroring
// the local error and deadline paths: a wedged or failed fresh CLI otherwise
// keeps its exempt slot. The stub chains to the run's own session when the
// result frame carried one, else to the job's previous session.
func TestAdoption_FreshUnsuccessfulResetsAndRefreshesStub(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		outcome   AdoptedRunOutcome
		wantChain []string
	}{
		{"cli_exited", AdoptedRunOutcome{Completed: false}, []string{"sess-prev"}},
		{"turn_failed", AdoptedRunOutcome{Completed: true, SessionID: "sess-a2", TurnErr: ErrTurnFailed}, []string{"sess-a2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := startAdoption(t, true, tc.outcome)
			c.settle(t)
			resets, regs := c.router.snapshot()
			if !reflect.DeepEqual(resets, []string{c.key}) {
				t.Fatalf("Reset calls = %v, want exactly [%s]", resets, c.key)
			}
			if len(regs) != 1 || !reflect.DeepEqual(regs[0].chainIDs, tc.wantChain) {
				t.Fatalf("stub registrations = %+v, want one chained to %v", regs, tc.wantChain)
			}
			c.ord.assertCountBefore(t, "reset", 1, "run-ended", "the reset must land while the gate is held")
			c.ord.assertCountBefore(t, "register-stub", 1, "run-ended", "the stub must be re-registered while the gate is held")
			c.router.assertGateHeld(t)
		})
	}
}

// A persistent-context adopted run keeps its session for the next tick to
// resume but releases the idle process, before the gate is released.
func TestAdoption_PersistentReleasesProcessWhileGateHeld(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		outcome AdoptedRunOutcome
	}{
		{"completed", AdoptedRunOutcome{Completed: true, Text: "ok", SessionID: "sess-keep"}},
		{"cli_exited", AdoptedRunOutcome{Completed: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := startAdoption(t, false, tc.outcome)
			c.settle(t)
			if got := c.router.releasedKeys(); !reflect.DeepEqual(got, []string{c.key}) {
				t.Fatalf("ReleaseProcess calls = %v, want exactly [%s]", got, c.key)
			}
			c.ord.assertCountBefore(t, "release", 1, "run-ended", "the persistent release must land while the gate is held")
			c.router.assertGateHeld(t)
			if resets, regs := c.router.snapshot(); len(resets) != 0 || len(regs) != 0 {
				t.Errorf("persistent run must keep its session; got Reset %v, stubs %+v", resets, regs)
			}
		})
	}
}

// Shutdown ending the wait cleans up nothing: the shim is meant to survive into
// the next process, which adopts or records the run; Router.Shutdown owns
// teardown of this one.
func TestAdoption_ShutdownLeavesSessionAlone(t *testing.T) {
	t.Parallel()
	for _, fresh := range []bool{true, false} {
		c := startAdoption(t, fresh, AdoptedRunOutcome{})
		c.s.Stop() // cancels stopCtx before the turn resolves; drains gcWG
		if n := c.rec.endedCount(); n != 1 {
			t.Fatalf("fresh=%v: want 1 run-ended event, got %d", fresh, n)
		}
		resets, regs := c.router.snapshot()
		if len(resets) != 0 || len(regs) != 0 || len(c.router.releasedKeys()) != 0 {
			t.Errorf("fresh=%v: shutdown must not touch the session; Reset %v, stubs %+v, releases %v",
				fresh, resets, regs, c.router.releasedKeys())
		}
	}
}

// A job deleted while its run was being adopted still has its session reset,
// but no stub is registered: that would resurrect a phantom sidebar row.
func TestAdoption_FreshDeletedJobResetsWithoutStub(t *testing.T) {
	t.Parallel()
	c := startAdoption(t, true, AdoptedRunOutcome{Completed: true, Text: "ok", SessionID: "sess-a1"})
	c.s.dropJobForTest(c.jobID)
	c.settle(t)
	resets, regs := c.router.snapshot()
	if !reflect.DeepEqual(resets, []string{c.key}) {
		t.Fatalf("Reset calls = %v, want exactly [%s]", resets, c.key)
	}
	if len(regs) != 0 {
		t.Errorf("deleted job got a stub re-registered: %+v", regs)
	}
}
