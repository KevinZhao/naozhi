package sysession

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestManager_StopOnHardFailPanicRecovered verifies #1286 (R20260527-COR-6):
// when cfg.OnHardFail panics on the deadline-exceeded branch, Stop must
// catch the panic and return normally rather than letting it tear out of
// stopOnce.Do — that path used to leak the watcher goroutine spawned by
// Stop because no one would close `done` and no one would cancel stopCtx.
//
// We:
//  1. Build a Manager with a daemon whose Tick deliberately ignores ctx
//     (sleeps long), so wg.Wait() can't return inside Stop's stopCtx.
//  2. Supply OnHardFail that bumps a counter then panics.
//  3. Call Stop with an already-expired stopCtx.
//  4. Assert Stop returns within a reasonable budget (no goroutine leak
//     tied to the panic) and OnHardFail was invoked exactly once.
func TestManager_StopOnHardFailPanicRecovered(t *testing.T) {
	pulse, tickFn := pulseTicker()

	// Daemon Tick ignores ctx and sleeps so wg.Wait() never returns
	// within stopCtx.
	d := &signalDaemon{
		name: "auto-titler",
		tickFn: func(_ context.Context, _ int32) (TickReport, error) {
			time.Sleep(2 * time.Second)
			return TickReport{}, nil
		},
	}
	withRegistry(t, []builtinDaemonFactory{
		{Name: "auto-titler", Build: func(deps DaemonDeps) (Daemon, error) { return d, nil }},
	})

	var hardFailCalls atomic.Int32
	router := newFakeRouter()
	m, err := NewManager(Config{
		Enabled:     true,
		TickTimeout: time.Second,
		Router:      router,
		Daemons: map[string]DaemonRuntimeConfig{
			"auto-titler": {Enabled: true, Tick: 1 * time.Millisecond},
		},
		NewTicker: tickFn,
		OnHardFail: func(code int) {
			hardFailCalls.Add(1)
			panic("simulated OnHardFail panic")
		},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	// Drive one tick to land inside the long-sleeping Tick.
	pulse <- time.Now()

	// Wait until daemon is in its blocking sleep so wg has a slot.
	deadline := time.Now().Add(time.Second)
	for d.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if d.calls.Load() == 0 {
		t.Fatal("daemon Tick never invoked; cannot exercise stopCtx deadline path")
	}

	// stopCtx that is already expired drives the deadline branch.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stopCancel()

	// Stop should not panic out — the recover frame in manager.go must
	// swallow the OnHardFail panic. Run with a watchdog so an actual leak
	// (Stop never returning) fails the test rather than hanging.
	stopReturned := make(chan struct{})
	var panickedOut atomic.Bool
	go func() {
		defer func() {
			if r := recover(); r != nil {
				panickedOut.Store(true)
			}
			close(stopReturned)
		}()
		m.Stop(stopCtx)
	}()

	select {
	case <-stopReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return; OnHardFail panic likely escaped (regression of #1286)")
	}
	if panickedOut.Load() {
		t.Fatal("Stop propagated OnHardFail panic to caller — recover frame missing")
	}
	if got := hardFailCalls.Load(); got != 1 {
		t.Errorf("OnHardFail call count = %d, want 1", got)
	}
}
