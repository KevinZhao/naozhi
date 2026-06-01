package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProcess_cachedProjectDir pins [R112714-PERF-2]: InitLinker must
// populate cachedProjectDir so notifyLinker never recomputes resolveProjectDir
// on every system/init event.
func TestProcess_cachedProjectDir(t *testing.T) {
	t.Parallel()
	cwd := "/home/ec2-user/workspace/naozhi"
	p := &Process{eventLog: NewEventLog(0)}
	p.InitLinker(cwd)

	wantSuffix := "-home-ec2-user-workspace-naozhi"
	wantFull := filepath.Join(os.Getenv("HOME"), ".claude", "projects", wantSuffix)
	if p.cachedProjectDir != wantFull {
		t.Errorf("cachedProjectDir = %q, want %q", p.cachedProjectDir, wantFull)
	}
	// Verify it matches resolveProjectDir(cwd) exactly.
	if got := resolveProjectDir(cwd); got != p.cachedProjectDir {
		t.Errorf("cachedProjectDir %q != resolveProjectDir %q", p.cachedProjectDir, got)
	}
}

// TestProcess_cachedProjectDir_empty ensures empty cwd yields empty cache
// (Resolve bails on empty projectDir — no regression).
func TestProcess_cachedProjectDir_empty(t *testing.T) {
	t.Parallel()
	p := &Process{eventLog: NewEventLog(0)}
	p.InitLinker("")
	if p.cachedProjectDir != "" {
		t.Errorf("cachedProjectDir should be empty for empty cwd, got %q", p.cachedProjectDir)
	}
}

// TestClaudeProjectsRoot_consistency verifies the sync.Once cache is
// consistent with a fresh call deriving from os.UserHomeDir.
func TestClaudeProjectsRoot_consistency(t *testing.T) {
	t.Parallel()
	got := claudeProjectsRoot()
	home := os.Getenv("HOME")
	want := filepath.Join(home, ".claude", "projects")
	if got != want {
		t.Errorf("claudeProjectsRoot() = %q, want %q", got, want)
	}
	// Second call must return the same cached value.
	if got2 := claudeProjectsRoot(); got2 != got {
		t.Errorf("claudeProjectsRoot() not idempotent: %q vs %q", got, got2)
	}
}
