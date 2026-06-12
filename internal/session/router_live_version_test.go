package session

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestRouter_CLIVersion_PrefersLiveObserved pins R20260612-global-version:
// Router.CLIVersion() (the source of the global dashboard banner's cli_version)
// returns the live version observed from a spawned process once available, so
// a host claude upgrade under a long-lived naozhi is reflected without restart.
// Before any process reports, it falls back to the spawn-time wrapper version.
func TestRouter_CLIVersion_PrefersLiveObserved(t *testing.T) {
	r := &Router{
		ss: sessionStore{sessions: make(map[string]*ManagedSession)},
	}
	w := cli.NewWrapper("/nonexistent/cli-binary", &cli.ClaudeProtocol{}, "claude")
	w.CLIVersion = "2.1.100" // simulate spawn-time detection
	r.bkStore.wrapper = w

	if got := r.CLIVersion(); got != "2.1.100" {
		t.Fatalf("CLIVersion before live observe = %q, want spawn-time 2.1.100", got)
	}

	// A spawned process reports the binary it actually exec'd (newer after a
	// host upgrade). Router.CLIVersion must now surface that.
	w.ObserveLiveVersion("2.1.174")
	if got := r.CLIVersion(); got != "2.1.174" {
		t.Fatalf("CLIVersion after live observe = %q, want live 2.1.174", got)
	}
}

// TestRouter_CLIVersion_EmptyWhenNoWrapper guards the unwired-router path.
func TestRouter_CLIVersion_EmptyWhenNoWrapper(t *testing.T) {
	r := &Router{ss: sessionStore{sessions: make(map[string]*ManagedSession)}}
	if got := r.CLIVersion(); got != "" {
		t.Fatalf("CLIVersion with no wrapper = %q, want empty", got)
	}
}
