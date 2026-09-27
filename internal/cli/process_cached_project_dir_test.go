package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/eventlog/ring"
	"github.com/naozhi/naozhi/internal/subagent"
)

// TestProcess_cachedProjectDir pins [R112714-PERF-2]: InitLinker must
// populate the project dir so notifyLinker never recomputes subagent.ProjectDir
// on every system/init event.
func TestProcess_cachedProjectDir(t *testing.T) {
	t.Parallel()
	cwd := "/home/ec2-user/workspace/naozhi"
	p := &Process{eventLog: ring.NewEventLog(0)}
	p.InitLinker(cwd)

	wantSuffix := "-home-ec2-user-workspace-naozhi"
	wantFull := filepath.Join(os.Getenv("HOME"), ".claude", "projects", wantSuffix)
	if got := p.linkerProjectDir(); got != wantFull {
		t.Errorf("project dir = %q, want %q", got, wantFull)
	}
	// Verify it matches subagent.ProjectDir(cwd) exactly.
	if got := subagent.ProjectDir(cwd); got != p.linkerProjectDir() {
		t.Errorf("project dir %q != subagent.ProjectDir %q", p.linkerProjectDir(), got)
	}
}

// TestProcess_cachedProjectDir_empty ensures empty cwd yields empty cache
// (Resolve bails on empty projectDir — no regression).
func TestProcess_cachedProjectDir_empty(t *testing.T) {
	t.Parallel()
	p := &Process{eventLog: ring.NewEventLog(0)}
	p.InitLinker("")
	if got := p.linkerProjectDir(); got != "" {
		t.Errorf("project dir should be empty for empty cwd, got %q", got)
	}
}

// TestClaudeProjectsRoot_consistency verifies claudeProjectsRoot derives
// from os.UserHomeDir correctly and is consistent across calls.
func TestClaudeProjectsRoot_consistency(t *testing.T) {
	t.Parallel()
	got := claudefs.ProjectsRoot(claudefs.DefaultDir())
	home := os.Getenv("HOME")
	want := filepath.Join(home, ".claude", "projects")
	if got != want {
		t.Errorf("claudefs.ProjectsRoot(claudefs.DefaultDir()) = %q, want %q", got, want)
	}
	// Two consecutive calls with the same HOME must agree.
	if got2 := claudefs.ProjectsRoot(claudefs.DefaultDir()); got2 != got {
		t.Errorf("claudefs.ProjectsRoot(claudefs.DefaultDir()) inconsistent: %q vs %q", got, got2)
	}
}
