package server

import (
	"strings"
	"testing"
)

// loadRootsBody returns the body of files_view.js' loadRoots function so
// assertions are windowed to the root-selection logic.
func loadRootsBody(t *testing.T) string {
	t.Helper()
	data, err := filesViewJS.ReadFile("static/files_view.js")
	if err != nil {
		t.Fatalf("read files_view.js: %v", err)
	}
	js := string(data)
	idx := strings.Index(js, "function loadRoots()")
	if idx < 0 {
		t.Fatal("loadRoots function not found in files_view.js")
	}
	rest := js[idx:]
	end := strings.Index(rest[1:], "\n  function ")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}

// TestFilesViewJS_LoadRoots_NoShortestPathHeuristic pins [R202606e-CR-007]:
// when no project reports is_root (include_root disabled), the browse-root
// fallback must be roots[0] (the operator's first-configured project), NOT
// the shortest-path heuristic. The old heuristic could pick an unrelated
// short-path project (e.g. "/x" over "/home/user/workspace"), dropping the
// operator into the wrong tree on multi-project deployments.
func TestFilesViewJS_LoadRoots_NoShortestPathHeuristic(t *testing.T) {
	t.Parallel()
	body := loadRootsBody(t)

	// The shortest-path heuristic compared .path lengths via reduce. Its
	// signature is the `.length <` comparison inside the reduce — assert it
	// is gone so a future edit can't silently reintroduce it.
	if strings.Contains(body, ".length <") {
		t.Error("loadRoots must not select the browse root by shortest .path length (#2282) — fall back to roots[0] instead")
	}

	// Positive: the roots[0] fallback must be present.
	if !strings.Contains(body, "state.roots[0]") {
		t.Error("loadRoots must fall back to state.roots[0] when no is_root project is present (#2282)")
	}
}
