package session_test

import (
	"os/exec"
	"strings"
	"testing"
)

// no_discovery_import_test.go pins the #3020 S15b invariant: internal/session
// must NOT depend on internal/discovery in production code.
//
// Background: discoveryHistoryLoader used to call discovery.LoadHistoryChainTailCtx
// directly, pinning a session→discovery edge. #3020 replaced it with
// claudeTranscriptLoader, which resolves the "claude" backend through
// history.PickFactory instead — the same inversion internal/cli already uses
// for its backend factories. This test runs `go list -deps` on session's
// production (non-test) closure and fails if internal/discovery reappears; a
// `go list` failure here means a broken module graph in a job that is
// already running `go test`, so it is a hard failure rather than a skip.
const (
	sessionPkg   = "github.com/naozhi/naozhi/internal/session"
	discoveryPkg = "github.com/naozhi/naozhi/internal/discovery"
)

func TestSession_NoDiscoveryInClosure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping `go list -deps` walk in -short mode")
	}
	out, err := exec.Command("go", "list", "-deps", sessionPkg).Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", sessionPkg, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == discoveryPkg {
			t.Fatalf("internal/session transitively imports %s — #3020 requires the "+
				"session→discovery edge stay cut. Resolve the \"claude\" backend through "+
				"history.PickFactory (claudeTranscriptLoader in router_core.go) instead of "+
				"importing discovery directly.", discoveryPkg)
		}
	}
}
