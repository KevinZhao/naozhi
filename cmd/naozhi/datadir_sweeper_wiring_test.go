package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/shim"
)

func plantOld(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSweeperActuallySweepsTheShimStateDir is the gate for the bug that shipped
// in v0.1.0: the shim-logs pass was wired from cfg.Session.Shim.StateDir, which
// is EMPTY unless the operator sets it, because shim.NewManager applies its
// ~/.naozhi/shims default to its own copy of the config. The pass was therefore
// inert for the default configuration — the very deployments the 909-file backlog
// came from — and the earlier tests missed it because their fixtures always set
// state_dir explicitly.
//
// This asserts behaviour rather than structure: a dead-pid log planted in the
// MANAGER's directory must actually be removed. Wire the raw config value back in
// and the pass gets Dir="" and removes nothing.
func TestSweeperActuallySweepsTheShimStateDir(t *testing.T) {
	shimDir := t.TempDir()
	mgr, err := shim.NewManager(shim.ManagerConfig{StateDir: shimDir})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	deadLog := plantOld(t, shimDir, shim.LogFilePrefix+"999999999.log")

	storeDir := t.TempDir()
	cliDebugLog := plantOld(t, filepath.Join(storeDir, "cli-debug"), "aaaaaaaaaaaaaaaa.log")

	// cfg.Session.Shim.StateDir is deliberately left EMPTY: that is the default
	// configuration and the case that used to break.
	cfg := &config.Config{}
	s := newDataDirSweeper(cfg, datadir.ForStore(filepath.Join(storeDir, "sessions.json")), mgr, "")
	got := s.RunOnce()

	if got["shim-logs"].Removed != 1 {
		t.Errorf("shim-logs removed %d, want 1 — the pass is not looking at the manager's state dir",
			got["shim-logs"].Removed)
	}
	if _, err := os.Stat(deadLog); err == nil {
		t.Error("the dead-pid shim log is still there")
	}
	if got["cli-debug"].Removed != 1 {
		t.Errorf("cli-debug removed %d, want 1", got["cli-debug"].Removed)
	}
	if _, err := os.Stat(cliDebugLog); err == nil {
		t.Error("the old cli-debug log is still there")
	}
	// sysWorkDir was "": the pass must not be registered at all, rather than
	// registered with an empty Dir where "swept nothing" looks normal.
	if _, ok := got["sys-sessions"]; ok {
		t.Errorf("sys-sessions registered with no work dir: %+v", got)
	}
}

// TestSweeperRegistersSysSessionsWhenItHasADir is the other half: the pass must
// appear once there is a directory for it.
func TestSweeperRegistersSysSessionsWhenItHasADir(t *testing.T) {
	mgr, err := shim.NewManager(shim.ManagerConfig{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	sysDir := t.TempDir()
	oldJSONL := plantOld(t, sysDir, "old.jsonl")

	cfg := &config.Config{}
	cfg.Sysession.Enabled = true
	s := newDataDirSweeper(cfg, datadir.ForStore(filepath.Join(t.TempDir(), "sessions.json")), mgr, sysDir)
	got := s.RunOnce()

	if got["sys-sessions"].Removed != 1 {
		t.Errorf("sys-sessions removed %d, want 1", got["sys-sessions"].Removed)
	}
	if _, err := os.Stat(oldJSONL); err == nil {
		t.Error("the old sys-sessions JSONL is still there")
	}
}

// TestSweeperSkipsShimPassWithoutAManager: a nil Manager must not yield a pass
// with an empty directory.
func TestSweeperSkipsShimPassWithoutAManager(t *testing.T) {
	cfg := &config.Config{}
	s := newDataDirSweeper(cfg, datadir.ForStore(filepath.Join(t.TempDir(), "sessions.json")), nil, "")
	if _, ok := s.RunOnce()["shim-logs"]; ok {
		t.Error("shim-logs registered despite a nil Manager")
	}
}
