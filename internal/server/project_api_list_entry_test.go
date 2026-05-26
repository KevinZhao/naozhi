package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/project"
)

// TestProjectsListEntry_JSONShape pins the wire format of the typed
// projectsListEntry struct introduced by R247-PERF-6 (#538) so the
// map[string]any → struct migration does not silently break dashboard.js
// or curl/IM consumers of GET /api/projects.
//
// Pre-existing TestDashboardJSON_Projects_ShapeContract pins per-project
// keys via a live handler+manager fixture; this test isolates the struct
// itself so the shape contract is grep-able from one place when a future
// edit adds/renames a field. The required keys mirror that test —
// name/path/planner_state/planner_model/config/favorite/git_remote_url/
// github must be present even when their values are zero so dashboard.js
// can `p.git_remote_url || ”` and `p.github === true` without an undef
// branch. R247-PERF-6 (#538).
func TestProjectsListEntry_JSONShape(t *testing.T) {
	t.Run("zero entry preserves required keys", func(t *testing.T) {
		// Empty/zero fields: omitempty on bool/string would drop these
		// from the JSON; the struct intentionally omits omitempty on the
		// shape-required fields so the dashboard contract holds even for
		// a freshly-created project with no git remote.
		buf, err := json.Marshal(projectsListEntry{
			Name: "demo", Path: "/tmp/demo",
			PlannerState: "none", PlannerModel: "claude-sonnet-4.6",
			Config: project.ProjectConfig{},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		got := string(buf)
		for _, want := range []string{
			`"name":"demo"`,
			`"path":"/tmp/demo"`,
			`"planner_state":"none"`,
			`"planner_model":"claude-sonnet-4.6"`,
			`"config":`,
			`"favorite":false`,
			`"git_remote_url":""`,
			`"github":false`,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("json = %s\nmissing %q", got, want)
			}
		}
		// Node carries omitempty since the local-only path never sets
		// it — must NOT appear when zero.
		if strings.Contains(got, `"node"`) {
			t.Errorf("json = %s\nmust omit zero `node` (multi-node merge stamps `local` before serialise)", got)
		}
	})

	t.Run("multi-node entry stamps node", func(t *testing.T) {
		buf, err := json.Marshal(projectsListEntry{
			Name: "demo", Path: "/tmp/demo", Node: "local",
			PlannerState: "running", PlannerModel: "m",
			Favorite: true, GitRemoteURL: "https://github.com/o/r", GitHub: true,
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		got := string(buf)
		for _, want := range []string{
			`"node":"local"`,
			`"favorite":true`,
			`"git_remote_url":"https://github.com/o/r"`,
			`"github":true`,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("json = %s\nmissing %q", got, want)
			}
		}
	})
}
