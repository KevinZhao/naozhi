package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/naozhi/naozhi/internal/ccmodels"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/session"
)

// TestSeedClaudeModelManifest_FillsOnlyUndeclaredClaudeBackends is what makes
// deleting cli.backends[].models safe: the popover has a list before any process
// is live, and an operator-declared list still wins.
func TestSeedClaudeModelManifest_FillsOnlyUndeclaredClaudeBackends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"availableModels":["claude-opus-5[1m]","claude-opus-5"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runtimes := map[string]session.BackendRuntime{
		"claude":     {},
		"kiro":       {},
		"claude-alt": {ConfiguredModels: []string{"operator-choice"}},
	}
	seedClaudeModelManifest(runtimes, path, "")

	want := []string{"claude-opus-5[1m]", "claude-opus-5"}
	if got := runtimes["claude"].ConfiguredModels; !reflect.DeepEqual(got, want) {
		t.Errorf("claude = %v, want %v", got, want)
	}
	if got := runtimes["kiro"].ConfiguredModels; got != nil {
		t.Errorf("kiro = %v, want nil — kiro reports its own manifest", got)
	}
	if got := runtimes["claude-alt"].ConfiguredModels; !reflect.DeepEqual(got, []string{"operator-choice"}) {
		t.Errorf("claude-alt = %v, want the operator's list untouched", got)
	}
}

func TestSeedClaudeModelManifest_TolerantOfMissingAndEmpty(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte(`{"availableModels":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"missing file":   filepath.Join(dir, "absent.json"),
		"no models":      empty,
		"no path at all": "",
	} {
		t.Run(name, func(t *testing.T) {
			runtimes := map[string]session.BackendRuntime{"claude": {}}
			seedClaudeModelManifest(runtimes, path, "")
			if got := runtimes["claude"].ConfiguredModels; got != nil {
				t.Errorf("ConfiguredModels = %v, want nil", got)
			}
		})
	}
}

// TestModelSyncTargets pins that the naozhi-owned file is synced only when it is
// the file naozhi spawns with; otherwise both cc's read the same local file.
func TestModelSyncTargets(t *testing.T) {
	claudeDir := t.TempDir()
	localPath := filepath.Join(claudeDir, "settings.json")

	cfg := &config.Config{}
	targets, err := modelSyncTargets(cfg, claudeDir)
	if err != nil {
		t.Fatalf("modelSyncTargets: %v", err)
	}
	if len(targets) != 1 || targets[0].path != localPath {
		t.Fatalf("disabled: targets = %+v, want just %s", targets, localPath)
	}

	isolated := filepath.Join(t.TempDir(), "naozhi-settings.json")
	cfg.NaozhiSettings = config.NaozhiSettingsConfig{Enabled: true, Path: isolated}
	targets, err = modelSyncTargets(cfg, claudeDir)
	if err != nil {
		t.Fatalf("modelSyncTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("enabled: targets = %+v, want two", targets)
	}
	if targets[0].path != localPath {
		t.Errorf("first target = %s, want the local file (it is the probe base)", targets[0].path)
	}
	if targets[1].path != isolated {
		t.Errorf("second target = %s, want %s", targets[1].path, isolated)
	}
	if targets[1].perm != 0o600 {
		t.Errorf("naozhi settings default perm = %o, want 600", targets[1].perm)
	}
}

// TestWriteTarget_BacksUpAndKeepsMode: a sync must be one `mv` from undone, and
// must not widen permissions on a file that may hold a token.
func TestWriteTarget_BacksUpAndKeepsMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	orig := []byte(`{"outputStyle":"Concise","availableModels":["stale"]}`)
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	plan := ccmodels.Plan{Aliases: []ccmodels.Alias{
		{Name: "claude-opus-5", Profile: "global.anthropic.claude-opus-5"},
	}}
	// perm 0644 on the target must lose to the file's own 0600.
	if err := writeTarget(syncTarget{path: path, perm: 0o644}, orig, plan); err != nil {
		t.Fatalf("writeTarget: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want the file's original 600", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var backups int
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			backups++
		}
	}
	if backups != 1 {
		t.Errorf("found %d backup files, want 1: %v", backups, entries)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := ccmodels.DiffSettings(got, plan)
	if err != nil {
		t.Fatalf("DiffSettings: %v", err)
	}
	if !diff.Empty() {
		t.Errorf("written file still differs from the plan: %+v", diff)
	}
}
