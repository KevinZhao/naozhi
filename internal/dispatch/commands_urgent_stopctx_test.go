package dispatch

// R20260531070014-ARCH-3 regression guard: /urgent goroutine must use
// mergeStopAndValues(d.stopCtx, ctx) rather than context.WithoutCancel(ctx).
//
// Before the fix, handleUrgentCommand called:
//   context.WithoutCancel(ctx)
// which discards all cancellation sources — including the long-lived
// d.stopCtx that signals graceful shutdown. On SIGTERM the /urgent goroutine
// would therefore run through its full internal totalTimeout (~5 min),
// causing systemd TimeoutStopSec breaches. The regular passthrough path
// (dispatch.go) fixed this in #1320 via mergeStopAndValues; /urgent had
// re-introduced the old pattern.
//
// This test asserts that mergeStopAndValues is wired correctly: the context
// produced for the /urgent goroutine must observe cancellation of stopCtx.

import (
	"context"
	"testing"
	"time"
)

// TestUrgent_SendCtxAbortsOnStopCtxCancel verifies that the context delivered
// to the /urgent goroutine (constructed via mergeStopAndValues) is cancelled
// when stopCtx is cancelled, and that it is NOT cancelled when only the
// per-request ctx is cancelled (the webhook handler returning must not kill
// the in-flight LLM turn).
func TestUrgent_SendCtxAbortsOnStopCtxCancel(t *testing.T) {
	t.Parallel()

	// Build the two context halves that handleUrgentCommand composes.
	stopCtx, cancelStop := context.WithCancel(context.Background())
	defer cancelStop()
	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()

	sendCtx := mergeStopAndValues(stopCtx, reqCtx)

	// Before any cancel: sendCtx must be live.
	select {
	case <-sendCtx.Done():
		t.Fatal("sendCtx already cancelled before any cancel called")
	default:
	}

	// Cancel the per-request ctx (webhook handler returns).
	// The in-flight /urgent goroutine must NOT be aborted.
	cancelReq()
	select {
	case <-sendCtx.Done():
		t.Fatal("sendCtx cancelled on reqCtx cancel — webhook return must not abort /urgent goroutine")
	case <-time.After(20 * time.Millisecond):
		// Good: still live after reqCtx cancel.
	}

	// Cancel stopCtx (SIGTERM / graceful shutdown).
	// The /urgent goroutine MUST now be abortable.
	cancelStop()
	select {
	case <-sendCtx.Done():
		// Correct: the goroutine can now be aborted on shutdown.
	case <-time.After(time.Second):
		t.Fatal("sendCtx not cancelled after stopCtx cancel — /urgent goroutine would survive SIGTERM (ARCH-3 regression)")
	}
}

// TestUrgent_SendCtxCarriesReqValues verifies that value lookups on the merged
// context still consult reqCtx, so per-request slog attributes and auth
// tokens remain accessible to the goroutine after reqCtx's deadline has
// cancelled (mergeStopAndValues copies values, not cancellation).
func TestUrgent_SendCtxCarriesReqValues(t *testing.T) {
	t.Parallel()

	type testKey struct{}
	stopCtx := context.Background()
	reqCtx := context.WithValue(context.Background(), testKey{}, "sentinel-value")

	sendCtx := mergeStopAndValues(stopCtx, reqCtx)

	got, _ := sendCtx.Value(testKey{}).(string)
	if got != "sentinel-value" {
		t.Errorf("sendCtx.Value(testKey{}) = %q, want %q — per-request values must survive in /urgent goroutine context", got, "sentinel-value")
	}
}
