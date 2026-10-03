package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// TestServerCaps_ReplyFooterDefaultsToRouterBackend: a session that pinned no
// backend is tagged with the router's default backend, not claude's.
func TestServerCaps_ReplyFooterDefaultsToRouterBackend(t *testing.T) {
	w := cli.NewWrapper("/nonexistent/kiro-cli", &cli.ClaudeProtocol{}, "kiro")
	r := session.NewRouter(session.RouterConfig{Wrapper: w, MaxProcs: 1})
	t.Cleanup(r.Shutdown)
	c := serverCaps{s: &Server{router: r}}
	if got := c.ReplyFooter(""); got != "kiro" {
		t.Errorf("ReplyFooter(\"\") = %q, want the default backend's tag kiro", got)
	}
	if got := c.ReplyFooter("claude"); got != "cc" {
		t.Errorf("ReplyFooter(claude) = %q, want cc", got)
	}
}
