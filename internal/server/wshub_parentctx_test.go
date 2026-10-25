package server

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

// TestNewHub_ParentCtxCancelPropagates locks the CTX1 contract: cancelling
// the parent context threaded via HubOptions.ParentCtx must cause h.ctx
// to become Done even when Shutdown() is never invoked. This closes the
// gap where a panic-early-exit path in main would forget to call Shutdown
// and leak send/push goroutines that observe h.ctx.Done().
func TestNewHub_ParentCtxCancelPropagates(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithCancel(context.Background())

	router := session.NewRouter(session.RouterConfig{})
	hub := newHubForTest(t, HubOptions{
		Router:    router,
		ParentCtx: parent,
	}, sendEngineOpts{})

	// Pre-cancel sanity: hub context must not be Done yet.
	select {
	case <-hub.ctx.Done():
		t.Fatal("hub.ctx is Done before parent cancel — derivation broken")
	default:
	}

	cancel()

	select {
	case <-hub.ctx.Done():
		// expected: parent-ctx cancel cascaded to h.ctx via WithCancel.
	case <-time.After(2 * time.Second):
		t.Fatal("hub.ctx not Done within 2s of parent cancel")
	}

	if err := hub.ctx.Err(); err != context.Canceled {
		t.Errorf("hub.ctx.Err() = %v, want context.Canceled", err)
	}

	// Shutdown must still be callable (h.cancel is idempotent) and must
	// not hang. We drive it through a goroutine bounded by a timer so a
	// regression that re-introduces a blocking Shutdown fails loudly.
	done := make(chan struct{})
	go func() {
		hub.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hub.Shutdown() hung after parent-ctx cancel")
	}
}

// TestNewHub_NilParentCtxFallsBackToBackground preserves the legacy
// behaviour for call sites that do not thread a parent ctx (tests and
// headless wiring). A nil ParentCtx must not panic and must yield a
// usable hub whose ctx is only Done after Shutdown.
func TestNewHub_NilParentCtxFallsBackToBackground(t *testing.T) {
	t.Parallel()

	router := session.NewRouter(session.RouterConfig{})
	hub := newHubForTest(t, HubOptions{
		Router: router,
		// ParentCtx intentionally omitted.
	}, sendEngineOpts{})

	select {
	case <-hub.ctx.Done():
		t.Fatal("hub.ctx unexpectedly Done on a background-derived hub")
	default:
	}

	hub.Shutdown()

	select {
	case <-hub.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("hub.ctx not Done after explicit Shutdown")
	}
}

// TestServer_AppCtxWiredToHub locks the Server-side half of the CTX1 contract:
// the app context must be live when NewHub reads it, and cancelling it must
// tear the Hub down even if Shutdown is never called.
//
// This was a source-level pin on the old shape (`s.appCtx = serveCtx` inside
// Start, `serveCtx, serveCancel := context.WithCancel(ctx)`). #2552 moved both
// the context and the Hub into buildServer, which makes the invariant
// structurally unbreakable rather than merely asserted: there is no longer a
// point in the Server's life where appCtx is nil while a Hub exists. So the
// test now drives the real constructor and observes the cascade — a stronger
// check that cannot rot when the wiring is rearranged again.
func TestServer_AppCtxWiredToHub(t *testing.T) {
	t.Parallel()

	srv := NewWithOptions(ServerOptions{
		Addr:   ":0",
		Router: session.NewRouter(session.RouterConfig{}),
	})

	// The pre-Start window is gone: both exist straight out of the constructor.
	if srv.appCtx == nil {
		t.Fatal("appCtx is nil after NewWithOptions — construction must not wait for Start (#2552)")
	}
	if srv.appCancel == nil {
		t.Fatal("appCancel is nil after NewWithOptions — nothing could cancel the app context")
	}
	if srv.hub == nil {
		t.Fatal("hub is nil after NewWithOptions — the whole point of #2552 is that this state does not exist")
	}

	select {
	case <-srv.hub.ctx.Done():
		t.Fatal("hub.ctx already Done straight after construction")
	default:
	}

	// CTX1: an app-level cancel must reach the Hub without Shutdown().
	srv.appCancel()
	select {
	case <-srv.hub.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("hub.ctx not Done within 2s of appCancel — HubOptions.ParentCtx is not wired to appCtx (CTX1)")
	}

	// Idempotent teardown must still work after the parent cancel.
	done := make(chan struct{})
	go func() {
		srv.hub.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hub.Shutdown() hung after appCancel")
	}
}
