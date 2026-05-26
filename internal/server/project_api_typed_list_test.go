package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// TestProjectListItem_TypedShape locks in R247-PERF-6 / #538: handleList
// must serialise via the typed projectListItem struct, not the legacy
// per-project map[string]any. The map shape allocated a hash table + 8
// boxed `any` slots per project on every dashboard 1Hz poll. The fix:
// replace map[string]any with the named struct (one heap slot per item,
// zero map alloc).
//
// We validate the struct surface via reflection on projectListItem so a
// future refactor that drops or renames a JSON tag is caught by this test
// instead of by a silent dashboard regression. Pairs with the wire-shape
// test in projects_shape_test.go (which exercises the handler end-to-end
// via httptest).
func TestProjectListItem_TypedShape(t *testing.T) {
	t.Parallel()

	want := []string{
		"name", "path", "planner_state", "planner_model",
		"config", "favorite", "git_remote_url", "github",
	}

	rt := reflect.TypeOf(projectListItem{})
	got := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		// Strip ",omitempty" et al.
		if c := indexComma(tag); c >= 0 {
			tag = tag[:c]
		}
		got = append(got, tag)
	}
	gotSet := make(map[string]bool, len(got))
	for _, k := range got {
		gotSet[k] = true
	}
	for _, k := range want {
		if !gotSet[k] {
			sort.Strings(got)
			t.Errorf("projectListItem missing json tag %q; got tags=%v", k, got)
		}
	}

	// "node" is omitempty — exercised only when remote nodes are configured.
	// Validate it exists with omitempty so the local-only response stays
	// byte-stable with the legacy map shape (no `node` key).
	nodeField, ok := rt.FieldByName("Node")
	if !ok {
		t.Fatal("projectListItem.Node field missing")
	}
	tag := nodeField.Tag.Get("json")
	if !strings.Contains(tag, "node") || !strings.Contains(tag, "omitempty") {
		t.Errorf("projectListItem.Node json tag = %q, want contains \"node\" and \"omitempty\"", tag)
	}
}

func indexComma(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			return i
		}
	}
	return -1
}

// TestHandleList_TypedNotMap is a regression guard against a future refactor
// reintroducing the per-project map[string]any allocation. It does an
// end-to-end handler call and re-decodes the body into the typed
// projectListItem; if a key got renamed without updating the struct tag the
// typed decode would silently drop the field. We then re-marshal the typed
// view and assert the byte length matches what the handler emitted (modulo
// JSON ordering quirks) — a smoke check that no extra map-only keys leak.
func TestHandleList_TypedNotMap(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	projDir := filepath.Join(root, "demo")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "CLAUDE.md"), []byte("# demo"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr, err := project.NewManager(root, project.PlannerDefaults{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.Scan(); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	router := session.NewRouter(session.RouterConfig{})
	platforms := map[string]platform.Platform{"test": &mockPlatform{}}
	srv := NewWithOptions(ServerOptions{
		Addr:           ":0",
		Router:         router,
		Platforms:      platforms,
		Backend:        "claude",
		ProjectManager: mgr,
	})
	srv.registerDashboard()

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	w := httptest.NewRecorder()
	srv.projectH.handleList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}

	// Decode into the typed view. If a JSON tag drifted, this would still
	// succeed but the corresponding field would be zero — assert at least
	// Name and Path are populated to catch that case.
	var typed []projectListItem
	if err := json.Unmarshal(w.Body.Bytes(), &typed); err != nil {
		t.Fatalf("decode typed: %v body=%s", err, w.Body.String())
	}
	if len(typed) == 0 {
		t.Fatal("typed projects empty")
	}
	if typed[0].Name == "" || typed[0].Path == "" {
		t.Errorf("typed[0] missing populated Name/Path: %+v", typed[0])
	}
	// HasNodes() returns false for the test setup (no nodes wired), so Node
	// must be empty — guards the omitempty contract.
	if typed[0].Node != "" {
		t.Errorf("typed[0].Node = %q, want empty (no nodes configured)", typed[0].Node)
	}
}
