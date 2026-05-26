package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveClaudeDir_HomeFallback confirms the helper joins ~/.claude
// when UserHomeDir succeeds. Pins R222-ARCH-9 / #724 single-source probe
// against future drift.
func TestResolveClaudeDir_HomeFallback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp) // os.UserHomeDir on linux honours $HOME
	got := resolveClaudeDir()
	want := filepath.Join(tmp, ".claude")
	if got != want {
		t.Errorf("resolveClaudeDir() = %q, want %q", got, want)
	}
}

// TestResolveClaudeProjectsDir_EnvOverride confirms CLAUDE_PROJECTS_DIR
// short-circuits the home probe so deployments that pin transcripts under
// /var/lib/claude do not need to symlink ~/.claude/projects. The env
// override existed before R222-ARCH-9 / #724; this test pins the
// short-circuit so the consolidation does not silently drop it.
func TestResolveClaudeProjectsDir_EnvOverride(t *testing.T) {
	override := t.TempDir()
	t.Setenv("CLAUDE_PROJECTS_DIR", override)
	// Even with HOME pointing elsewhere, the env override wins.
	t.Setenv("HOME", t.TempDir())
	got := resolveClaudeProjectsDir()
	if got != override {
		t.Errorf("resolveClaudeProjectsDir() with override = %q, want %q",
			got, override)
	}
}

// TestResolveClaudeProjectsDir_HomeFallback confirms ~/.claude/projects is
// the default when the env override is absent.
func TestResolveClaudeProjectsDir_HomeFallback(t *testing.T) {
	if err := os.Unsetenv("CLAUDE_PROJECTS_DIR"); err != nil {
		t.Fatalf("unset CLAUDE_PROJECTS_DIR: %v", err)
	}
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	got := resolveClaudeProjectsDir()
	want := filepath.Join(tmp, ".claude", "projects")
	if got != want {
		t.Errorf("resolveClaudeProjectsDir() = %q, want %q", got, want)
	}
	if !strings.HasSuffix(got, filepath.Join(".claude", "projects")) {
		t.Errorf("resolveClaudeProjectsDir() = %q missing .claude/projects suffix", got)
	}
}
