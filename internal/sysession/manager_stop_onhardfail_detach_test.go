package sysession

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestManager_StopOnHardFailDetachReturns pins R237-ARCH-6 (#585): when
// an embedder overrides OnHardFail to a no-op (the documented "graceful
// detach" path — let the caller decide whether to take the host process
// down rather than the default os.Exit(2)), Stop must return cleanly to
// the caller within stopCtx instead of blocking on the leaked watcher
// goroutine. This is the embedder contract Stop's godoc and Config.OnHardFail
// promise — without a pinning test, a refactor that re-introduces a
// blocking wait after OnHardFail (e.g. an extra m.wg.Wait() in the
// deadline branch) would regress libraries that wrap naozhi as a
// dependency. The watcher goroutine is intentionally leaked per
// R234-GO-5; this test only asserts Stop's caller-visible return.
//
// Setup mirrors TestManager_StopOnHardFailPanicRecovered:
//  1. Daemon Tick ignores ctx and sleeps long so wg.Wait can't drain
//     within stopCtx.
//  2. OnHardFail just bumps a counter and returns (the no-op detach
//     contract).
//  3. Stop with an already-expired stopCtx must return promptly; not
//     hang on m.wg.Wait or any other internal join.
func TestManager_StopOnHardFailDetachReturns(t *testing.T) {
	pulse, tickFn := pulseTicker()

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
	var hardFailCode atomic.Int32
	router := newFakeRouter()
	m, err := NewManager(Config{
		Enabled:     true,
		TickTimeout: time.Second,
		Router:      router,
		Daemons: map[string]DaemonRuntimeConfig{
			"auto-titler": {Enabled: true, Tick: 1 * time.Millisecond},
		},
		NewTicker: tickFn,
		// Embedder-style override: detach the daemon and return rather
		// than tear down the host process. #585 (R237-ARCH-6) proposal.
		OnHardFail: func(code int) {
			hardFailCalls.Add(1)
			hardFailCode.Store(int32(code))
		},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	// Drive one tick so wg has at least one in-flight slot — otherwise
	// Stop's wg.Wait would close `done` immediately and we wouldn't
	// exercise the stopCtx-deadline branch at all.
	pulse <- time.Now()
	deadline := time.Now().Add(time.Second)
	for d.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if d.calls.Load() == 0 {
		t.Fatal("daemon Tick never invoked; cannot exercise stopCtx deadline path")
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stopCancel()

	stopReturned := make(chan struct{})
	go func() {
		m.Stop(stopCtx)
		close(stopReturned)
	}()

	select {
	case <-stopReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after OnHardFail no-op; embedder graceful-detach contract regressed (#585)")
	}
	if got := hardFailCalls.Load(); got != 1 {
		t.Errorf("OnHardFail call count = %d, want 1", got)
	}
	if got := hardFailCode.Load(); got != 2 {
		t.Errorf("OnHardFail code = %d, want 2 (the documented stop-deadline exit code)", got)
	}
}
