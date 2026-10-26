package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/osutil"
)

// doctor reports backend.Profile.HistoryDir and the history factories read
// backendHistoryDirs(); these tests keep the two derivations from drifting
// (#2668 is what two disagreeing derivations of one fact cost).

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

// TestBackendHistoryDirs_OneEntryPerRegisteredBackend: the router's transcript
// directories come from the profile registry, so every registered backend is
// wired without a per-backend line in main.go. A backend without a HistoryDir
// is skipped by backendHistoryDirs and fails the length check: its "load
// earlier" would silently return nothing after a restart.
func TestBackendHistoryDirs_OneEntryPerRegisteredBackend(t *testing.T) {
	backend.EnsureDefaults()
	got := backendHistoryDirs()
	all := backend.All()
	if len(got) != len(all) {
		t.Errorf("backendHistoryDirs() has %d entries, want one per registered backend (%d): %v", len(got), len(all), got)
	}
	for _, p := range all {
		if want := osutil.ExpandHome(p.HistoryDir); got[p.ID] != want {
			t.Errorf("backendHistoryDirs()[%q] = %q, want %q", p.ID, got[p.ID], want)
		}
	}
}
