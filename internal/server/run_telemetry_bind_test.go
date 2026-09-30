package server

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/runtelemetry"
	"github.com/naozhi/naozhi/internal/session"
)

// The server binds the Hub's run-event broadcaster to the relay cron and
// sysession were built with.
func TestServer_BindsRunTelemetry(t *testing.T) {
	relay := &runtelemetry.Relay{}
	_ = NewWithOptions(ServerOptions{
		Addr:         ":0",
		Router:       session.NewRouter(session.RouterConfig{}),
		RunTelemetry: relay,
		Platforms:    map[string]platform.Platform{"test": &mockPlatform{}},
		Backend:      "claude",
	})
	defer func() {
		if p, _ := recover().(string); !strings.Contains(p, "bound twice") {
			t.Error("the server left the run-telemetry relay unbound")
		}
	}()
	relay.Bind(nil)
}
