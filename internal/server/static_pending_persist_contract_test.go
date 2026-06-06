package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_PendingPersistWiring pins the frontend half of the
// new-session cwd-fallback fix. dashboard.js is not exercised by a JS unit
// runner, so this Go contract guards the persistence + eager-bind wiring
// against a silent refactor that would re-open the bug where a
// reload-before-first-send dropped the chosen workspace and the session
// landed in defaultCWD (workspace root).
func TestDashboardJS_PendingPersistWiring(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	for _, want := range []string{
		// The two persistence helpers exist.
		"function persistPending()",
		"function restorePending()",
		// The durable storage key + the eager-bind helper exist.
		"PENDING_LS_KEY",
		"function eagerBindWorkspace(key, workspace, node)",
		// The eager-bind hits the new backend endpoint.
		"'/api/sessions/bind'",
		// Boot rehydrates pending sessions before the first fetch/send.
		"restorePending();",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("dashboard.js missing pending-persist wiring: %q", want)
		}
	}

	// persistPending must be called at the create funnels AND on send
	// consumption AND in removePendingSession — at least 6 call sites. A
	// refactor that drops most of them would silently regress durability.
	if n := strings.Count(js, "persistPending()"); n < 6 {
		t.Errorf("persistPending() call sites = %d; want >= 6 (create funnels + send-consume + removePendingSession + definition)", n)
	}

	// eager-bind must fire from at least the two project-create funnels
	// (doCreateInProject + doCreateSession). The definition is one more ref.
	if n := strings.Count(js, "eagerBindWorkspace("); n < 3 {
		t.Errorf("eagerBindWorkspace( references = %d; want >= 3 (definition + 2 create funnels)", n)
	}

	// The fetchSessions backend-reconciliation loop must persist the durable
	// blob after dropping now-real keys (a single batched persistPending), so a
	// stale pending entry can't re-inject a ghost card on the next reload.
	// (Batched rather than per-key removePendingSession to avoid M redundant
	// full-blob serializations in one poll tick.)
	if !strings.Contains(js, "if (reconciledAny) persistPending();") {
		t.Error("fetchSessions reconciliation must persist the pending blob once after dropping backend-known keys")
	}

	// restorePending must guard against a hand-edited blob: only absolute /
	// home-relative workspaces are accepted. A regression that dropped the
	// check would let a junk blob inject arbitrary cwds into the send payload.
	if !strings.Contains(js, "v.ws[0] !== '/' && v.ws[0] !== '~'") {
		t.Error("restorePending missing absolute-path guard on the restored workspace")
	}
}
