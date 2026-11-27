package server

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

// TestServer_AppCtxWiredToHub locks the Server-side half of the CTX1 contract:
// the app context must be live when NewHub reads it, and cancelling it must
// tear the Hub down even if Shutdown is never called.
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
