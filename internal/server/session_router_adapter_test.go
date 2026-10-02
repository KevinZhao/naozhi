package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// TestSessionRouterView_CLIFactsFromBackends: the adapter's two CLI facts are
// the default backend's, and the version follows a live observation the way
// the dashboard banner needs (R20260612-global-version).
func TestSessionRouterView_CLIFactsFromBackends(t *testing.T) {
	w := cli.NewWrapper("/nonexistent/cli-binary", &cli.ClaudeProtocol{}, "claude")
	w.CLIName = "claude-code"
	w.CLIVersion = "2.1.100"
	r := session.NewRouter(session.RouterConfig{Wrapper: w, MaxProcs: 1})
	t.Cleanup(r.Shutdown)
	v := sessionRouterView{r}

	if got := v.CLIName(); got != "claude-code" {
		t.Errorf("CLIName() = %q, want claude-code", got)
	}
	if got := v.CLIVersion(); got != "2.1.100" {
		t.Errorf("CLIVersion() = %q, want the spawn-time 2.1.100", got)
	}
	w.ObserveLiveVersion("2.1.174")
	if got := v.CLIVersion(); got != "2.1.174" {
		t.Errorf("CLIVersion() after a live observation = %q, want 2.1.174", got)
	}
}
