package project

import (
	"context"
	"testing"
)

// TestProjectHandlers_RestartCtx_NilFallback pins the contract that
// `restartCtx` falls back to context.Background() when SetBaseContext
// has not been wired. R247-ARCH-15 (#650): the prior `ctxFunc` closure
// returned Background when `s.hub` was nil; the new field-based design
// must preserve that fallback so test paths that build Handlers
// by hand (without going through registerDashboard) keep working.
func TestProjectHandlers_RestartCtx_NilFallback(t *testing.T) {
	h := &Handlers{}
	got := h.restartCtx()
	if got == nil {
		t.Fatal("restartCtx() returned nil — must fall back to Background")
	}
	// Background is never Done; a Done parent here would mean some test
	// ancestor leaked a cancelled context into the fallback path.
	select {
	case <-got.Done():
		t.Fatalf("restartCtx() fallback context already Done; err=%v", got.Err())
	default:
	}
}

// TestProjectHandlers_SetBaseContext pins the contract that
// SetBaseContext stores the supplied context and restartCtx returns
// it verbatim. registerDashboard relies on this to thread `s.hub.ctx`
// (the long-lived process context) into the planner-restart timeout
// once the Hub finishes constructing.
func TestProjectHandlers_SetBaseContext(t *testing.T) {
	h := &Handlers{}
	type ctxKey struct{}
	want := context.WithValue(context.Background(), ctxKey{}, "marker")
	h.SetBaseContext(want)

	got := h.restartCtx()
	if got != want {
		t.Fatalf("restartCtx() returned %p, want %p (the wired baseCtx)", got, want)
	}
	if v, _ := got.Value(ctxKey{}).(string); v != "marker" {
		t.Fatalf("restartCtx() lost ctxKey value: got %q want %q", v, "marker")
	}
}

// TestProjectHandlers_SetBaseContext_CancelPropagates pins that
// cancellation on the wired baseCtx propagates through restartCtx. A
// process shutdown cancels `s.hub.ctx`; the planner-restart timeout
// must observe that so an in-flight ResetAndRecreate aborts instead
// of running the full 30s deadline past process exit.
func TestProjectHandlers_SetBaseContext_CancelPropagates(t *testing.T) {
	h := &Handlers{}
	parent, cancel := context.WithCancel(context.Background())
	h.SetBaseContext(parent)

	got := h.restartCtx()
	select {
	case <-got.Done():
		t.Fatal("restartCtx() Done before parent cancel — leaked cancelled ctx?")
	default:
	}

	cancel()
	select {
	case <-got.Done():
		// expected
	default:
		t.Fatal("restartCtx() did not observe parent cancel")
	}
	if got.Err() != context.Canceled {
		t.Fatalf("restartCtx().Err() = %v, want context.Canceled", got.Err())
	}
}
