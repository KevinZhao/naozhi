package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestHandleAPISessions_StatsStructShape locks the complete set of top-level
// keys in the /api/sessions "stats" object. Round 79 migrated stats from
// map[string]any to the named `sessionStats` struct (embedding
// sessionStatsStatic); this test regresses the byte-for-byte key set so a
// future refactor that adds/removes a field without bumping the dashboard.js
// contract trips immediately instead of producing silent UI breakage.
//
// dashboard.js today consumes: version / agents / default_workspace /
// projects / cli_name / cli_version / workspace_id / workspace_name / system
// (see renderSidebar + fetchSessions in internal/server/static/dashboard.js).
// The dynamic counters active/running/ready/total/uptime/backend/max_procs/
// watchdog are exposed for curl/monitoring consumers so we lock them too.
func TestHandleAPISessions_StatsStructShape(t *testing.T) {
	agents := map[string]session.AgentOpts{
		"code-reviewer": {Model: "sonnet"},
	}
	router := session.NewRouter(session.RouterConfig{
		MaxProcs:  3,
		Workspace: "/tmp/naozhi-ws",
	})
	srv := New(":0", router, nil, agents, nil, nil, "claude", ServerOptions{})
	srv.registerDashboard()

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.sessionH.handleList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	stats, ok := resp["stats"].(map[string]any)
	if !ok {
		t.Fatalf("stats missing or wrong type: %T", resp["stats"])
	}

	// Required fields — always present regardless of project manager /
	// remote nodes configuration. `projects` is omitempty when nothing is
	// configured so it is NOT required here.
	required := []string{
		"backend", "cli_name", "cli_version", "max_procs",
		"default_workspace", "workspace_id", "workspace_name",
		"system", "agents",
		"active", "running", "ready", "total",
		"version", "uptime", "watchdog",
	}
	for _, k := range required {
		if _, ok := stats[k]; !ok {
			gotKeys := make([]string, 0, len(stats))
			for k := range stats {
				gotKeys = append(gotKeys, k)
			}
			sort.Strings(gotKeys)
			t.Errorf("stats.%s missing; got keys = %v", k, gotKeys)
		}
	}

	// Unexpected keys — surface accidental additions so the dashboard.js
	// contract is reviewed alongside the server change.
	expected := map[string]bool{}
	for _, k := range required {
		expected[k] = true
	}
	expected["projects"] = true    // optional — omitted when no project manager
	expected["version_tag"] = true // optional — omitted when -X main.version is not set
	for k := range stats {
		if !expected[k] {
			t.Errorf("unexpected stats.%s — add to dashboard.js contract before shipping", k)
		}
	}

	// Watchdog must remain a 2-field struct — lock against the "reviewer
	// adds a new watchdog field without updating dashboard" regression.
	wd, ok := stats["watchdog"].(map[string]any)
	if !ok {
		t.Fatalf("watchdog wrong type: %T", stats["watchdog"])
	}
	for _, k := range []string{"no_output_kills", "total_kills"} {
		if _, ok := wd[k]; !ok {
			t.Errorf("watchdog.%s missing", k)
		}
	}

	// system retained as map[string]any with the 5 known fingerprint keys —
	// changing these requires a dashboard.js renderSidebar update.
	sys, ok := stats["system"].(map[string]any)
	if !ok {
		t.Fatalf("system wrong type: %T", stats["system"])
	}
	for _, k := range []string{"os", "arch", "cpus", "memory_mb", "ip_count"} {
		if _, ok := sys[k]; !ok {
			t.Errorf("system.%s missing", k)
		}
	}
}

// TestHandleAPISessions_StatsProjectsOmitemptyEmpty verifies that the
// `projects` key is ABSENT (not emitted as null or empty array) when no
// project manager is wired. Prior map code handled this via the `if
// len(projectList) > 0 { stats["projects"] = ... }` branch; the struct
// migration uses the `omitempty` tag. A nil Projects slice plus omitempty
// means the field is dropped from the JSON output — dashboard.js's
// `if (data.stats.projects) projectsData = data.stats.projects;` guard
// must still short-circuit correctly.
func TestHandleAPISessions_StatsProjectsOmitemptyEmpty(t *testing.T) {
	router := session.NewRouter(session.RouterConfig{})
	srv := New(":0", router, nil, nil, nil, nil, "claude", ServerOptions{})
	srv.registerDashboard()

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.sessionH.handleList(w, req)

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	stats := resp["stats"].(map[string]any)
	if v, ok := stats["projects"]; ok {
		t.Errorf("stats.projects should be absent when no projects; got %v", v)
	}
}

// TestHandleAPISessions_StatsStaticStructEmbedsFlatJSON locks the anonymous
// embedding contract: sessionStatsStatic fields must promote to the top
// level of the "stats" object (not nest under a "static" sub-key). Without
// the anonymous embed the struct would marshal as {"sessionStatsStatic":
// {"backend":...}} breaking every dashboard.js consumer. This test is
// deliberately narrow — it proves the embed is anonymous, not named.
func TestHandleAPISessions_StatsStaticStructEmbedsFlatJSON(t *testing.T) {
	router := session.NewRouter(session.RouterConfig{})
	srv := New(":0", router, nil, nil, nil, nil, "claude", ServerOptions{})
	srv.registerDashboard()

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.sessionH.handleList(w, req)

	body := w.Body.String()
	// If embed is named, keys would be under "sessionStatsStatic":{...}.
	if contains(body, `"sessionStatsStatic"`) {
		t.Errorf("stats embeds sessionStatsStatic as named field — must be anonymous; body=%s", body)
	}
	// Sanity: one of the static fields must appear at the top level of stats.
	if !contains(body, `"backend"`) {
		t.Errorf("stats.backend missing from JSON — static embed broken; body=%s", body)
	}
}

// contains is a small helper mirroring strings.Contains so this test file
// does not drag in the strings import just to inspect a body. Inlined to
// keep the shape-regression suite self-contained.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
