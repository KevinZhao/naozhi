package main

import (
	"os/user"
	"path/filepath"
	"testing"
)

// TestIsolatedInstance: only ~/.naozhi in the account's own home is not
// isolated, whatever $HOME says.
func TestIsolatedInstance(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Skip("no account home:", err)
	}
	t.Setenv("HOME", t.TempDir())
	if isolatedInstance(filepath.Join(u.HomeDir, ".naozhi")) {
		t.Error("the account's own ~/.naozhi counted as isolated")
	}
	for _, root := range []string{t.TempDir(), filepath.Join(t.TempDir(), ".naozhi")} {
		if !isolatedInstance(root) {
			t.Errorf("%s not counted as isolated", root)
		}
	}
}
