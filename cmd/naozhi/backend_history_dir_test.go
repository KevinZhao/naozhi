package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/osutil"
)

// "Where does backend X keep its transcripts" had two independent derivations:
// `naozhi doctor` read backend.Profile.HistoryDir, while the history factories got
// the same paths as literals in main.go, threaded through RouterConfig into
// history.Wiring. Nothing tied them together, so changing one left the other
// reporting a directory nobody reads.
//
// #2668 is what that costs when it happens to a different fact: two derivations of
// a backend's spawn defaults, one of them wrong, producing a spurious hard DRIFT
// that told the operator to restart a healthy session.
//
// kiro and codex now read the profile. claude cannot: the profile says
// "~/.claude/projects/" while the factory is given "~/.claude" and appends the
// projects segment itself (claudefs owns that layout). So the invariant is
// asserted rather than the strings being made identical.

// TestBackendHistoryDir_MatchesProfile pins that the value the history factories
// receive is the profile's, expanded — not a literal that happens to agree today.
func TestBackendHistoryDir_MatchesProfile(t *testing.T) {
	backend.EnsureDefaults()
	for _, id := range []string{"kiro", "codex"} {
		p, ok := backend.Get(id)
		if !ok {
			t.Fatalf("backend %q is not registered; EnsureDefaults did not run", id)
		}
		if p.HistoryDir == "" {
			t.Errorf("backend %q has no HistoryDir, so the history factory now gets \"\" and "+
				"\"load earlier\" silently returns nothing after a restart", id)
			continue
		}
		if got, want := backendHistoryDir(id), osutil.ExpandHome(p.HistoryDir); got != want {
			t.Errorf("backendHistoryDir(%q) = %q, want %q (the profile's value, expanded)", id, got, want)
		}
	}
}

// TestClaudeHistoryDir_AgreesWithTheProfile is the claude half of the same
// invariant, expressed structurally because the two values legitimately differ by
// one path segment: RouterConfig.ClaudeDir is the ~/.claude root and
// claudefs.ProjectsRoot appends "projects", which is what the profile spells out.
//
// If either side moves — the profile's path, or claudefs' layout — this fails
// instead of doctor quietly reporting a directory the reader does not use.
func TestClaudeHistoryDir_AgreesWithTheProfile(t *testing.T) {
	backend.EnsureDefaults()
	p, ok := backend.Get("claude")
	if !ok {
		t.Fatal("claude backend is not registered")
	}
	if p.HistoryDir == "" {
		t.Fatal("claude profile has no HistoryDir")
	}

	// What main.go passes as RouterConfig.ClaudeDir.
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")

	// What the reader actually looks under, and what the profile claims — both
	// normalised so a trailing slash is not the thing under test.
	fromReader := filepath.Clean(claudefs.ProjectsRoot(claudeDir))
	fromProfile := filepath.Clean(strings.Replace(p.HistoryDir, "~", home, 1))

	if fromReader != fromProfile {
		t.Errorf("the claude transcript directory has two answers:\n"+
			"  reader  (claudefs.ProjectsRoot of RouterConfig.ClaudeDir) = %q\n"+
			"  profile (backend.Profile.HistoryDir, what doctor reports)  = %q\n"+
			"one of them moved; doctor would now name a directory nothing reads",
			fromReader, fromProfile)
	}
}

// TestEveryBackendProfileDeclaresAHistoryDir catches a backend added without one.
// The failure is silent: the factory receives "", treats it as "no fallback
// history", and "load earlier" returns nothing after a restart with no error
// anywhere.
func TestEveryBackendProfileDeclaresAHistoryDir(t *testing.T) {
	backend.EnsureDefaults()
	for _, id := range []string{"claude", "kiro", "codex"} {
		p, ok := backend.Get(id)
		if !ok {
			t.Errorf("backend %q is not registered", id)
			continue
		}
		if p.HistoryDir == "" {
			t.Errorf("backend %q declares no HistoryDir. Add one: it is both what doctor "+
				"reports and, for everything but claude, what the history factory reads.", id)
		}
		if strings.HasPrefix(p.HistoryDir, "~") && osutil.ExpandHome(p.HistoryDir) == p.HistoryDir {
			t.Errorf("backend %q HistoryDir %q did not expand; the factory would get a literal tilde",
				id, p.HistoryDir)
		}
	}
}

// TestBackendHistoryDir_UnknownBackendIsEmpty: the factories already treat "" as
// "no fallback history for this backend", so an unregistered id must not become a
// bogus path.
func TestBackendHistoryDir_UnknownBackendIsEmpty(t *testing.T) {
	backend.EnsureDefaults()
	if got := backendHistoryDir("no-such-backend"); got != "" {
		t.Errorf("backendHistoryDir(unknown) = %q, want empty", got)
	}
}
