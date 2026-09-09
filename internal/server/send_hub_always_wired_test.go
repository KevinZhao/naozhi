package server

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestSendWithBroadcast_HubAlwaysWired replaces the three Headless tests
// (#2634). ServerOptions.Headless promised a Server "wired without a
// dashboard Hub on purpose"; after #2552 no constructor path could produce
// one — buildDashboard runs unconditionally — so the flag documented a state
// that did not exist and its fail-loud gate was reachable only from a
// hand-built &Server{}. What is worth pinning is the fact that made the flag
// dead: every constructed Server has a Hub with an engine, and the IM / cron
// send entry reaches that engine.
func TestSendWithBroadcast_HubAlwaysWired(t *testing.T) {
	t.Parallel()
	router := session.NewRouter(session.RouterConfig{})
	srv := NewWithOptions(ServerOptions{Addr: ":0", Router: router, Backend: "claude"})
	t.Cleanup(srv.appCancel)

	if srv.hub == nil || srv.hub.engine == nil {
		t.Fatal("NewWithOptions produced a Server without a Hub / send engine — the hub-less mode #2634 removed has come back")
	}
	// The nil-session guard is the only branch left before delegation.
	if _, err := srv.sendWithBroadcast(context.Background(), "k", nil, "hi", nil, nil); err == nil {
		t.Fatal("sendWithBroadcast with nil session must return an error")
	}
}
