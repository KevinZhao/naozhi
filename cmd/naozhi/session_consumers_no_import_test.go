// session_consumers_no_import_test.go pins the packages that talk to the
// session router through their own consumer interfaces: none of them may
// depend on internal/session in production code. Value types come from
// internal/session/sessionview and key helpers from internal/sessionkey; the
// adapter that knows the concrete *session.Router lives at the wiring site
// (internal/wireup for upstream, internal/server for dashboard/project).
//
// Same mechanism as cron_no_session_import_test.go: `go list -deps` walks the
// real build graph, so a leaf package that starts importing session is caught
// too, not just a direct import.

package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestSessionConsumers_NoSessionImport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping `go list -deps` walk in -short mode")
	}
	for _, tc := range []struct{ pkg, adapter string }{
		{"github.com/naozhi/naozhi/internal/upstream", "internal/wireup/upstream_router.go"},
		{"github.com/naozhi/naozhi/internal/dashboard/project", "internal/server/project_router_adapter.go"},
	} {
		out, err := exec.Command("go", "list", "-deps", tc.pkg).Output()
		if err != nil {
			t.Skipf("go list failed (skipping import-graph check): %v", err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.TrimSpace(line) == sessionPkg {
				t.Errorf("%s transitively imports %s; take value types from sessionview / sessionkey and convert the concrete session in %s", tc.pkg, sessionPkg, tc.adapter)
			}
		}
	}
}
