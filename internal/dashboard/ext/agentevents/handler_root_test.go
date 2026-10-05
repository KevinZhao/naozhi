package agentevents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/claudefs"
)

// TestNew_ProjectsRoot: the server-injected root wins (it is the WS
// tailer's root too), and without one New falls back to the resolved
// default ~/.claude/projects.
func TestNew_ProjectsRoot(t *testing.T) {
	t.Parallel()
	if got := New(Deps{ProjectsRoot: "/srv/claude/projects"}).ProjectsRoot(); got != "/srv/claude/projects" {
		t.Errorf("New(ProjectsRoot set).ProjectsRoot() = %q, want the injected root", got)
	}
	want := claudefs.ResolvedProjectsRoot(claudefs.DefaultDir())
	if got := New(Deps{}).ProjectsRoot(); got != want {
		t.Errorf("New(ProjectsRoot unset).ProjectsRoot() = %q, want the default %q", got, want)
	}
}

// TestJsonlPathUnderAllowedRoot_CaseInsensitiveFS mirrors the server
// package's table for the tailer gate: both gates answer alike on a
// case-insensitive filesystem.
func TestJsonlPathUnderAllowedRoot_CaseInsensitiveFS(t *testing.T) {
	t.Parallel()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "projects")
	jsonl := filepath.Join(root, "-ws", "agent-a1.jsonl")
	if err := os.MkdirAll(filepath.Dir(jsonl), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsonl, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	upper := filepath.Join(base, strings.ToUpper("projects"))
	if _, err := os.Stat(upper); err != nil {
		t.Skip("case-sensitive filesystem")
	}
	if err := os.Mkdir(filepath.Join(base, "PROJECTSX"), 0o700); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join(upper, "-ws", "agent-a1.jsonl"), true},
		{filepath.Join(upper, "-ws", "agent-not-yet-written.jsonl"), true},
		{upper, false},
		{filepath.Join(base, "PROJECTSX", "agent-a1.jsonl"), false},
	}
	for _, c := range cases {
		if got := jsonlPathUnderAllowedRoot(c.path, root); got != c.want {
			t.Errorf("jsonlPathUnderAllowedRoot(%q, %q) = %v, want %v", c.path, root, got, c.want)
		}
	}
}
