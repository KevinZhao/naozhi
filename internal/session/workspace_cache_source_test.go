package session

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestWorkspaceCache_SingleSourceContract pins R245-ARCH-32 (#883): workspace
// state lives in four places — Router.workspace (default), Router.workspaceOverrides
// (per-chat override), ManagedSession.workspace (per-session cache) and
// Process.cwd (spawn-time arg). #883's invariant is that ManagedSession.workspace
// is a PASSIVE MIRROR of the router's resolved decision, never an independent
// source: it is only ever written by router-internal lifecycle/discovery/restore
// code that already resolved the authoritative value via resolveSpawnParamsLocked
// (whose precedence is pinned by workspace_resolver_contract_test.go).
//
// A regression that re-introduces a fourth independent source — e.g. a
// ManagedSession method that mutates s.workspace on its own, or a non-router
// site that calls setWorkspace from outside the resolved path — would re-open
// the drift #883 flagged: the dashboard / attachment-gc / spawn paths could
// then disagree about a session's cwd.
//
// We assert that every production setWorkspace WRITE call site lives in one of
// the sanctioned router-internal files. The setter is already unexported (so
// no other package can call it), but a future intra-package "quick fix" could
// still add a write from, say, a ManagedSession self-mutation helper; this test
// keeps the write surface enumerated and reviewed.
func TestWorkspaceCache_SingleSourceContract(t *testing.T) {
	// Files allowed to WRITE the per-session workspace cache. Each is a
	// router-internal path that has already resolved the authoritative value:
	//   - router_lifecycle.go: spawn + ResetAndRecreate (from resolveSpawnParamsLocked)
	//   - router_discovery.go: shim/disk discovery reconciliation
	//   - router_core.go:      restore from a persisted store entry
	//   - managed_identity.go: the setWorkspace definition itself
	allowed := map[string]struct{}{
		"router_lifecycle.go": {},
		"router_discovery.go": {},
		"router_core.go":      {},
		"managed_identity.go": {},
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read session pkg dir: %v", err)
	}
	// Match a setWorkspace call: `<recv>.setWorkspace(` — the write surface.
	call := regexp.MustCompile(`\.setWorkspace\(`)
	var offenders []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue // test fixtures legitimately set workspace directly
		}
		body, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !call.Match(body) {
			continue
		}
		if _, ok := allowed[name]; !ok {
			offenders = append(offenders, name)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("R245-ARCH-32 (#883) contract broken: setWorkspace called from "+
			"non-sanctioned file(s) %v. ManagedSession.workspace must stay a passive "+
			"mirror of the router-resolved decision (resolveSpawnParamsLocked); do not "+
			"add a new independent workspace source. If this write is legitimately "+
			"router-internal, add the file to the allowed set with a justification.",
			offenders)
	}
}
