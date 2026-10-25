package shim

import (
	"os"
	"runtime"
	"testing"
)

// shortSocketDir returns a short-path temporary directory safe for Unix domain
// sockets. macOS caps sun_path at 104 bytes (Linux: 108); t.TempDir() embeds
// the test name, and on macOS sits under /var/folders/..., which together can
// exceed that budget. The directory is allocated directly under /tmp on macOS
// and under os.TempDir() elsewhere, without the test name.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	root := os.TempDir()
	if runtime.GOOS == "darwin" {
		root = "/tmp"
	}
	dir, err := os.MkdirTemp(root, "naozhi-shim-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
