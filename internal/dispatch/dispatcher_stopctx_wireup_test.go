package dispatch

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

// TestNewDispatcher_StopCtxDefault_BackgroundFallback pins the documented
// fallback for headless / test wiring that omits cfg.StopCtx: NewDispatcher
// must seed d.stopCtx with context.Background() so the BuildHandler hot
// path can dereference d.stopCtx unconditionally without a nil check.
//
// Without this pin a future refactor that drops the `if cfg.StopCtx !=
// nil { ... } else { context.Background() }` branch would crash the very
// first inbound message at the mergeStopAndValues call site (panic on nil
// cancelSrc). (#1320)
func TestNewDispatcher_StopCtxDefault_BackgroundFallback(t *testing.T) {
	t.Parallel()
	d, err := NewDispatcher(DispatcherConfig{
		Dedup:              platform.NewDedup(0),
		AllowMissingSender: true,
		// StopCtx intentionally omitted to exercise the fallback branch.
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	if d.stopCtx == nil {
		t.Fatal("d.stopCtx is nil; mergeStopAndValues would panic on the first inbound message")
	}
	// Background never cancels and has no deadline — both observable from
	// the standard library contract, no internal poke required.
	if _, ok := d.stopCtx.Deadline(); ok {
		t.Error("default stopCtx must be Background (no deadline)")
	}
	if d.stopCtx.Err() != nil {
		t.Errorf("default stopCtx already done: %v", d.stopCtx.Err())
	}
}

// TestNewDispatcher_StopCtxPropagated confirms an explicit StopCtx flows
// into the Dispatcher field — not silently overwritten by Background.
// This pins the production wireup path: server.Start passes its service
// ctx, and the BuildHandler passthrough branch later merges that into
// the per-webhook ctx via mergeStopAndValues. (#1320)
func TestNewDispatcher_StopCtxPropagated(t *testing.T) {
	t.Parallel()
	stopCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := NewDispatcher(DispatcherConfig{
		Dedup:              platform.NewDedup(0),
		AllowMissingSender: true,
		StopCtx:            stopCtx,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	// Cancel the explicit ctx and observe propagation through the
	// dispatcher field — this is the load-bearing graceful-shutdown
	// path that #1320 restores after WithoutCancel orphaned it.
	cancel()
	select {
	case <-d.stopCtx.Done():
	default:
		t.Fatal("d.stopCtx did not see cancellation from the explicit cfg.StopCtx")
	}
}
