package shim

import (
	"os"
	"runtime"
	"testing"
)

// shortSocketDir returns a short-path temporary directory safe for Unix domain
// sockets. macOS caps sun_path at 104 bytes (Linux: 108); the default
// t.TempDir() root under /var/folders/... plus the test name and /NNN/ suffix
// exceeds that budget. On macOS we allocate directly under /tmp; elsewhere we
// defer to t.TempDir().
func shortSocketDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "darwin" {
		return t.TempDir()
	}
	dir, err := os.MkdirTemp("/tmp", "naozhi-shim-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
