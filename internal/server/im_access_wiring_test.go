package server

import (
	"context"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/imauth"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

// ServerOptions.IMAccess reaches the dispatcher the IM adapters call: a
// stranger's direct message is refused at the handler the server hands out.
func TestServerOptions_IMAccessReachesDispatcher(t *testing.T) {
	plat := newParityPlatform(false)
	srv, _ := buildServerWithHandlers(ServerOptions{
		Addr:      ":0",
		Router:    session.NewRouter(session.RouterConfig{}),
		Platforms: map[string]platform.Platform{parityPlatformName: plat},
		Backend:   "claude",
		IMAccess: &imauth.Policy{Rules: map[string]imauth.Rule{
			parityPlatformName: {Allowed: map[string]struct{}{"alice": {}}},
		}},
	})
	t.Cleanup(func() {
		srv.hub.Shutdown()
		srv.appCancel()
	})
	srv.dispatcher.BuildHandler()(context.Background(), platform.IncomingMessage{
		Platform: parityPlatformName, EventID: "e1", UserID: "eve",
		ChatID: parityChatID, ChatType: "direct", Text: "hello",
	})
	if got := plat.allReplies(); len(got) != 1 || !strings.Contains(got[0], "ID: eve") {
		t.Fatalf("replies = %q, want the access refusal", got)
	}
}
