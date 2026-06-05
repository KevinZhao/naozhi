package sysession

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// recordingBroadcaster captures the runtelemetry events Manager produces so
// tests can assert the Subsystem tag, OwnerID, RunID pairing, and the enum
// mapping that #1723 Phase 1 moved into the sysession package. Goroutine-safe
// because emitRun{Started,Ended} fire from the per-daemon tick goroutine while
// the test reads from its own goroutine.
type recordingBroadcaster struct {
	mu      sync.Mutex
	started []runtelemetry.RunStartedEvent
	ended   []runtelemetry.RunEndedEvent
}

func (r *recordingBroadcaster) BroadcastRunStarted(ev runtelemetry.RunStartedEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = append(r.started, ev)
}

func (r *recordingBroadcaster) BroadcastRunEnded(ev runtelemetry.RunEndedEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended = append(r.ended, ev)
}

func (r *recordingBroadcaster) snapshot() ([]runtelemetry.RunStartedEvent, []runtelemetry.RunEndedEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := append([]runtelemetry.RunStartedEvent(nil), r.started...)
	e := append([]runtelemetry.RunEndedEvent(nil), r.ended...)
	return s, e
}

// newTelemetryManager builds a Manager whose single test daemon's Tick is
// fully under the caller's control via tickFn. RunOnStart=true with a 1h tick
// so the one Tick fires deterministically at startup without a ticker pulse.
func newTelemetryManager(t *testing.T, name string, tickFn func() (TickReport, error)) *Manager {
	t.Helper()
	d := &signalDaemon{
		name: name,
		tickFn: func(_ context.Context, _ int32) (TickReport, error) {
			return tickFn()
		},
	}
	withRegistry(t, []builtinDaemonFactory{
		{Name: name, Build: func(deps DaemonDeps) (Daemon, error) { return d, nil }},
	})
	_, tickerFn := pulseTicker()
	m, err := NewManager(Config{
		Enabled:     true,
		TickTimeout: 200 * time.Millisecond,
		Router:      newFakeRouter(),
		Daemons: map[string]DaemonRuntimeConfig{
			name: {Enabled: true, Tick: time.Hour, RunOnStart: true},
		},
		NewTicker: tickerFn,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

// runOneAndWait starts the manager (RunOnStart fires one Tick), waits until the
// broadcaster has observed exactly one ended event, then stops cleanly.
func runOneAndWait(t *testing.T, m *Manager, rec *recordingBroadcaster) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop(context.Background())

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ended := rec.snapshot(); len(ended) >= 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no RunEnded event observed within deadline")
}

// TestManager_EmitsSysessionSubsystem pins that both run events carry
// Subsystem=SubsystemSysession and that the started/ended pair shares the same
// non-empty RunID and OwnerID (the daemon name).
func TestManager_EmitsSysessionSubsystem(t *testing.T) {
	const name = "auto-titler"
	m := newTelemetryManager(t, name, func() (TickReport, error) {
		return TickReport{Acted: 1}, nil
	})
	rec := &recordingBroadcaster{}
	m.SetTelemetry(rec)

	runOneAndWait(t, m, rec)

	started, ended := rec.snapshot()
	if len(started) != 1 {
		t.Fatalf("started events = %d, want 1", len(started))
	}
	if len(ended) != 1 {
		t.Fatalf("ended events = %d, want 1", len(ended))
	}
	s, e := started[0], ended[0]

	if s.Subsystem != runtelemetry.SubsystemSysession {
		t.Errorf("started Subsystem = %q, want %q", s.Subsystem, runtelemetry.SubsystemSysession)
	}
	if e.Subsystem != runtelemetry.SubsystemSysession {
		t.Errorf("ended Subsystem = %q, want %q", e.Subsystem, runtelemetry.SubsystemSysession)
	}
	if s.OwnerID != name || e.OwnerID != name {
		t.Errorf("OwnerID started=%q ended=%q, want %q for both", s.OwnerID, e.OwnerID, name)
	}
	if s.RunID == "" {
		t.Error("started RunID is empty")
	}
	if s.RunID != e.RunID {
		t.Errorf("RunID mismatch: started=%q ended=%q (must pair 1:1)", s.RunID, e.RunID)
	}
	if s.Trigger != runtelemetry.TriggerScheduled {
		t.Errorf("started Trigger = %q, want scheduled", s.Trigger)
	}
	if e.State != runtelemetry.RunStateSucceeded {
		t.Errorf("ended State = %q, want succeeded", e.State)
	}
	if e.ErrorClass != runtelemetry.ErrClassNone {
		t.Errorf("ended ErrorClass = %q, want none", e.ErrorClass)
	}
}

// TestManager_NilBroadcasterIsSafe pins that a Manager which never had
// SetTelemetry called (the default after construction, and the test/no-WS
// deployment shape) ticks without panicking and records the run internally.
func TestManager_NilBroadcasterIsSafe(t *testing.T) {
	const name = "auto-titler"
	m := newTelemetryManager(t, name, func() (TickReport, error) {
		return TickReport{Acted: 1}, nil
	})
	// Deliberately do NOT call SetTelemetry — telemetry pointer stays nil.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop(context.Background())

	// Wait for the internal run record (recordRun runs even with nil
	// broadcaster) — the absence of a panic is the primary assertion.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st := m.Inspector()
		if len(st) == 1 && st[0].LastRun != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no run recorded with nil broadcaster (or it panicked)")
}

// TestManager_RunCountersBumpBroadcastIndependent pins that the symmetric
// run-count expvar counters (naozhi_sysession_run_{started,ended}_total) each
// increment by exactly 1 per driven Tick EVEN WITH A NIL BROADCASTER. This is
// the whole point of placing the .Add before the nil-broadcaster early return
// in emitRun{Started,Ended}: the counter must track run lifecycle, not the
// broadcast path, mirroring cron's R230C-GO-15. Snapshots the global counters
// before/after rather than asserting absolutes (expvar.Int is process-global).
func TestManager_RunCountersBumpBroadcastIndependent(t *testing.T) {
	const name = "auto-titler"
	startBefore := metrics.SysessionRunStartedTotal.Value()
	endBefore := metrics.SysessionRunEndedTotal.Value()

	m := newTelemetryManager(t, name, func() (TickReport, error) {
		return TickReport{Acted: 1}, nil
	})
	// Deliberately do NOT call SetTelemetry — telemetry pointer stays nil so
	// the broadcast path is never taken, isolating the counter bump.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop(context.Background())

	// Wait for the internal run record (recordRun runs even with nil
	// broadcaster); by then both emit helpers have fired.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st := m.Inspector()
		if len(st) == 1 && st[0].LastRun != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := metrics.SysessionRunStartedTotal.Value() - startBefore; got != 1 {
		t.Errorf("SysessionRunStartedTotal delta = %d, want 1 (must bump with nil broadcaster)", got)
	}
	if got := metrics.SysessionRunEndedTotal.Value() - endBefore; got != 1 {
		t.Errorf("SysessionRunEndedTotal delta = %d, want 1 (must bump with nil broadcaster)", got)
	}
}

// TestManager_SetTelemetryNilClears pins that passing nil to SetTelemetry
// reverts to no-broadcast mode without panicking on a subsequent emit.
func TestManager_SetTelemetryNilClears(t *testing.T) {
	const name = "auto-titler"
	m := newTelemetryManager(t, name, func() (TickReport, error) {
		return TickReport{Acted: 1}, nil
	})
	rec := &recordingBroadcaster{}
	m.SetTelemetry(rec)
	m.SetTelemetry(nil) // clear before any tick fires

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop(context.Background())

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st := m.Inspector()
		if len(st) == 1 && st[0].LastRun != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if started, ended := rec.snapshot(); len(started) != 0 || len(ended) != 0 {
		t.Errorf("cleared broadcaster still received events: started=%d ended=%d", len(started), len(ended))
	}
}

// TestManager_TimeoutMapsToDeadlineExceeded drives a daemon whose Tick exceeds
// the per-tick budget so the run classifies as DaemonErrorClassTimeout
// ("timeout"), and pins that the emitted runtelemetry event normalises that to
// ErrClassDeadlineExceeded ("deadline_exceeded") — the load-bearing mapping the
// dashboard keys off. End-to-end through emitRunEnded, not just the pure map.
func TestManager_TimeoutMapsToDeadlineExceeded(t *testing.T) {
	const name = "auto-titler"
	d := &signalDaemon{
		name: name,
		tickFn: func(ctx context.Context, _ int32) (TickReport, error) {
			// Block past the (short) tick budget so tickCtx expires; return
			// the ctx error so classifyError routes to timeout.
			<-ctx.Done()
			return TickReport{}, fmt.Errorf("daemon hit budget: %w", ctx.Err())
		},
	}
	withRegistry(t, []builtinDaemonFactory{
		{Name: name, Build: func(deps DaemonDeps) (Daemon, error) { return d, nil }},
	})
	_, tickerFn := pulseTicker()
	m, err := NewManager(Config{
		Enabled:     true,
		TickTimeout: 30 * time.Millisecond,
		Router:      newFakeRouter(),
		Daemons: map[string]DaemonRuntimeConfig{
			name: {Enabled: true, Tick: time.Hour, RunOnStart: true},
		},
		NewTicker: tickerFn,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rec := &recordingBroadcaster{}
	m.SetTelemetry(rec)

	runOneAndWait(t, m, rec)

	_, ended := rec.snapshot()
	if len(ended) != 1 {
		t.Fatalf("ended events = %d, want 1", len(ended))
	}
	e := ended[0]
	if e.State != runtelemetry.RunStateTimedOut {
		t.Errorf("ended State = %q, want timed_out", e.State)
	}
	if e.ErrorClass != runtelemetry.ErrClassDeadlineExceeded {
		t.Errorf("ended ErrorClass = %q, want %q (timeout must normalise)",
			e.ErrorClass, runtelemetry.ErrClassDeadlineExceeded)
	}
	if string(e.ErrorClass) == string(DaemonErrorClassTimeout) {
		t.Errorf("ended ErrorClass leaked raw sysession wire string %q", e.ErrorClass)
	}
}

// TestManager_UpstreamErrorClass pins that an organic (non-ctx, non-validation)
// Tick error emits ErrClassSysessionUpstream and RunStateFailed on the wire.
func TestManager_UpstreamErrorClass(t *testing.T) {
	const name = "auto-titler"
	m := newTelemetryManager(t, name, func() (TickReport, error) {
		return TickReport{}, fmt.Errorf("CLI exploded")
	})
	rec := &recordingBroadcaster{}
	m.SetTelemetry(rec)

	runOneAndWait(t, m, rec)

	_, ended := rec.snapshot()
	if len(ended) != 1 {
		t.Fatalf("ended events = %d, want 1", len(ended))
	}
	if ended[0].State != runtelemetry.RunStateFailed {
		t.Errorf("ended State = %q, want failed", ended[0].State)
	}
	if ended[0].ErrorClass != runtelemetry.ErrClassSysessionUpstream {
		t.Errorf("ended ErrorClass = %q, want %q", ended[0].ErrorClass, runtelemetry.ErrClassSysessionUpstream)
	}
}

// TestManager_SetTelemetryRaceWithTick exercises SetTelemetry stores
// concurrently with the per-daemon tick goroutine reading the pointer via
// emitRun{Started,Ended}. Run under -race, this guards the atomic.Pointer
// invariant the #1723 refactor depends on (the seam cron already proved).
// The ticker is pulsed repeatedly so emit fires while SetTelemetry churns.
func TestManager_SetTelemetryRaceWithTick(t *testing.T) {
	const name = "auto-titler"
	d := &signalDaemon{
		name: name,
		tickFn: func(_ context.Context, _ int32) (TickReport, error) {
			return TickReport{Acted: 1}, nil
		},
	}
	withRegistry(t, []builtinDaemonFactory{
		{Name: name, Build: func(deps DaemonDeps) (Daemon, error) { return d, nil }},
	})
	pulse := make(chan time.Time, 1)
	tickerFn := func(_ time.Duration) (<-chan time.Time, func()) {
		return pulse, func() {}
	}
	m, err := NewManager(Config{
		Enabled:     true,
		TickTimeout: 200 * time.Millisecond,
		Router:      newFakeRouter(),
		Daemons: map[string]DaemonRuntimeConfig{
			name: {Enabled: true, Tick: time.Hour},
		},
		NewTicker: tickerFn,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop(context.Background())

	done := make(chan struct{})
	// Writer goroutine: churn SetTelemetry between two broadcasters and nil.
	go func() {
		defer close(done)
		a := &recordingBroadcaster{}
		b := &recordingBroadcaster{}
		for i := 0; i < 500; i++ {
			switch i % 3 {
			case 0:
				m.SetTelemetry(a)
			case 1:
				m.SetTelemetry(b)
			default:
				m.SetTelemetry(nil)
			}
		}
	}()

	// Reader side: drive ticks so emitRun{Started,Ended} read the pointer
	// concurrently with the writer churn above.
	for i := 0; i < 50; i++ {
		select {
		case pulse <- time.Now():
		default:
		}
		time.Sleep(time.Millisecond)
	}
	<-done
}
