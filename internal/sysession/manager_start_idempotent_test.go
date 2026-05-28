package sysession

import (
	"context"
	"testing"
	"time"
)

// TestManager_StartIsIdempotent locks the post-#1377 contract: a second
// Start call MUST return silently (logged at warn) rather than panic.
// Cron's scheduler.Start uses the same CAS-gate idempotent shape; this
// test guarantees sysession agrees with it. Issue #1377.
func TestManager_StartIsIdempotent(t *testing.T) {
	pulse, tickFn := pulseTicker()
	_ = pulse

	d := &signalDaemon{name: "auto-titler"}
	withRegistry(t, []builtinDaemonFactory{
		{Name: "auto-titler", Build: func(deps DaemonDeps) (Daemon, error) { return d, nil }},
	})

	router := newFakeRouter()
	m, err := NewManager(Config{
		Enabled:     true,
		TickTimeout: 100 * time.Millisecond,
		Router:      router,
		Daemons: map[string]DaemonRuntimeConfig{
			"auto-titler": {Enabled: true, Tick: 50 * time.Millisecond},
		},
		NewTicker: tickFn,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.Start(ctx)

	// Second Start: must NOT panic. Pre-#1377 this aborted the test
	// process with "sysession: Manager.Start called twice".
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("second Start panicked, expected idempotent no-op; got %v", r)
		}
	}()
	m.Start(ctx)
	m.Start(ctx) // belt+braces: third call must also be a no-op

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	m.Stop(stopCtx)
}

// TestManager_StartIdempotentWhenDisabled verifies the disabled-Manager
// fast path also tolerates repeated Start without panic — the pre-#1377
// code returned early on m.enabled==false anyway, but we pin the
// behaviour so future refactors don't accidentally regress it. Issue #1377.
func TestManager_StartIdempotentWhenDisabled(t *testing.T) {
	m, err := NewManager(Config{Enabled: false})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("disabled Start panicked: %v", r)
		}
	}()
	m.Start(context.Background())
	m.Start(context.Background())
	m.Stop(context.Background())
}
