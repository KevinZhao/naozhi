package discovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestListWorkspaceJSONL_Empty(t *testing.T) {
	if got := ListWorkspaceJSONL("", "/foo"); got != nil {
		t.Fatalf("empty claudeDir: got %v want nil", got)
	}
	if got := ListWorkspaceJSONL("/tmp", ""); got != nil {
		t.Fatalf("empty workspace: got %v want nil", got)
	}
}

func TestListWorkspaceJSONL_NonexistentSlug(t *testing.T) {
	dir := t.TempDir()
	if got := ListWorkspaceJSONL(dir, "/nonexistent/path"); got != nil {
		t.Fatalf("nonexistent slug: got %v want nil", got)
	}
}

func TestListWorkspaceJSONL_HappyPath(t *testing.T) {
	claudeDir, ws, slug := newWorkspaceFixture(t)
	projDir := filepath.Join(claudeDir, "projects", slug)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"00000000-0000-4000-8000-000000000001",
		"00000000-0000-4000-8000-000000000002",
	}
	for i, id := range want {
		writeJSONL(t, projDir, id, "user-line\n", time.Now().Add(time.Duration(i)*time.Second))
	}

	got := ListWorkspaceJSONL(claudeDir, ws)
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d (%v)", len(got), len(want), got)
	}
	idsGot := map[string]bool{}
	for _, e := range got {
		idsGot[e.SessionID] = true
		if e.Mtime <= 0 {
			t.Errorf("entry %s has non-positive mtime %d", e.SessionID, e.Mtime)
		}
	}
	for _, id := range want {
		if !idsGot[id] {
			t.Errorf("missing id %s in result", id)
		}
	}
}

func TestListWorkspaceJSONL_FiltersInvalidIDs(t *testing.T) {
	claudeDir, ws, slug := newWorkspaceFixture(t)
	projDir := filepath.Join(claudeDir, "projects", slug)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	validID := "00000000-0000-4000-8000-000000000001"
	writeJSONL(t, projDir, validID, "x\n", time.Now())
	// Non-UUID name — must be filtered.
	writeJSONL(t, projDir, "not-a-uuid", "x\n", time.Now())
	// Empty file (size==0) — must be filtered (cachedJSONLFileInfo skips).
	emptyPath := filepath.Join(projDir, "00000000-0000-4000-8000-00000000000a.jsonl")
	if err := os.WriteFile(emptyPath, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	got := ListWorkspaceJSONL(claudeDir, ws)
	if len(got) != 1 || got[0].SessionID != validID {
		t.Fatalf("expected only %s, got %+v", validID, got)
	}
}

// newWorkspaceFixture returns a claudeDir, workspace path, and the slug
// derived from the workspace. The workspace path is just a string —
// ListWorkspaceJSONL does not Stat it, only joins to the projects/<slug>
// path under claudeDir.
func newWorkspaceFixture(t *testing.T) (claudeDir, workspace, slug string) {
	t.Helper()
	claudeDir = t.TempDir()
	workspace = "/home/test/workspace/foo"
	slug = strings.ReplaceAll(workspace, "/", "-")
	return
}

func writeJSONL(t *testing.T, projDir, sessionID, content string, mtime time.Time) {
	t.Helper()
	path := filepath.Join(projDir, sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}
