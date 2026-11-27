package server

import (
	"path/filepath"
	"testing"
)

// CLAUDE_PROJECTS_DIR wins over the home probe; without it the projects root
// is ~/.claude/projects.
func TestResolveClaudeProjectsDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_PROJECTS_DIR", "")
	if got, want := resolveClaudeProjectsDir(), filepath.Join(home, ".claude", "projects"); got != want {
		t.Errorf("home fallback = %q, want %q", got, want)
	}
	override := t.TempDir()
	t.Setenv("CLAUDE_PROJECTS_DIR", override)
	if got := resolveClaudeProjectsDir(); got != override {
		t.Errorf("with override = %q, want %q", got, override)
	}
}
