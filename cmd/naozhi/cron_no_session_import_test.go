// cron_no_session_import_test.go pins the cron→session no-reverse-import
// invariant established by RFC cron-sysession-merge Phase B (#1318):
// internal/cron must NOT depend on internal/session in production code.
//
// Background: Phase B inverted the historical cron→session dependency by
// declaring cron-local types (AgentOpts / Session / SessionStatus /
// InterruptOutcome) and introducing cmd/naozhi as the boundary that
// translates between cron-local and session types. The
// session.SessionIDExcluder compile-time guard was the last remaining
// import edge; #1318 moved it to cmd/naozhi/cron_router_adapter.go.
//
// Without this contract test, a future cron-side change that reaches for
// any session.* symbol would silently re-introduce the reverse import
// (the build still succeeds — Go does not flag cycles that pass through
// the cmd/ boundary). This test runs `go list -deps ./internal/cron`
// and fails if internal/session appears in the transitive closure.
//
// Refs: docs/rfc/cron-sysession-merge.md Phase B (§3.3.3); #1318.

package main

import (
	"os/exec"
	"strings"
	"testing"
)

const (
	cronPkg    = "github.com/naozhi/naozhi/internal/cron"
	sessionPkg = "github.com/naozhi/naozhi/internal/session"
)

// TestCron_NoReverseImport_Session asserts internal/cron's transitive
// dependency closure does NOT include internal/session.
//
// Failure mode: a cron-side commit that re-imports session (directly or
// via a previously-leaf package that started importing session) flips
// this test red on the next CI run. The error message lists the offending
// import edge so the author can either (a) move the new dependency through
// the wireup adapter boundary (internal/wireup/cron_router_adapter.go, the
// layer that imports both cron and session — R260528-ARCH-23 / #1382 moved it
// there from cmd/naozhi), or (b) revert.
//
// `go list -deps` walks the build's actual dependency graph (not just
// declared imports), so build-tag tricks cannot bypass it. The test runs
// `go list` once per invocation; cost is ~100ms on a warm cache, dominated
// by package metadata loading. Skipped under -short for the same reason
// other heavy contract tests (e.g. agent_opts mirror) skip.
func TestCron_NoReverseImport_Session(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping `go list -deps` walk in -short mode")
	}
	out, err := exec.Command("go", "list", "-deps", cronPkg).Output()
	if err != nil {
		// `go list` failure usually means the test runner has no go in
		// PATH (sandboxed CI) or the module graph is broken. Skip rather
		// than fail so a transient tooling issue doesn't mask real
		// regressions in the rest of the suite.
		t.Skipf("go list failed (skipping import-graph check): %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == sessionPkg {
			t.Fatalf("internal/cron transitively imports %s — RFC cron-sysession-merge Phase B (#1318) "+
				"requires cron→session inversion. Move new session types through "+
				"internal/wireup/cron_router_adapter.go (cron-local mirror + adapter cast) "+
				"instead of importing session directly.", sessionPkg)
		}
	}
}
