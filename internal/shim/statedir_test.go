package shim

import (
	"os"
	"path/filepath"
	"testing"
)

// TestManagerStateDirAppliesTheDefault is the gate for the bug this file's
// accessor exists to close: NewManager defaults an empty StateDir to
// ~/.naozhi/shims on its own copy of the config, so a caller that read
// cfg.Session.Shim.StateDir instead got "". The datadir.Sweeper retention pass
// did exactly that and silently swept nothing for every deployment that did not
// set session.shim.state_dir explicitly — which is the default configuration,
// and is where the 909-file backlog that motivated the pass came from.
func TestManagerStateDirAppliesTheDefault(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	m, err := NewManager(ManagerConfig{StateDir: ""})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	want := filepath.Join(home, ".naozhi", "shims")
	if got := m.StateDir(); got != want {
		t.Errorf("StateDir() = %q, want the applied default %q", got, want)
	}
	if m.StateDir() == "" {
		t.Error("StateDir() is empty; a sweep pass given this would be a silent no-op")
	}
}

// TestManagerStateDirEchoesAnExplicitValue: the accessor must not invent a path
// when the operator set one.
func TestManagerStateDirEchoesAnExplicitValue(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m, err := NewManager(ManagerConfig{StateDir: dir})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if got := m.StateDir(); got != dir {
		t.Errorf("StateDir() = %q, want %q", got, dir)
	}
}
