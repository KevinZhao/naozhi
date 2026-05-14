package shim

import (
	"os"
	"path/filepath"
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

// findRepoRoot walks upward from this test file until it finds go.mod.
// Used by repo-wide regression contracts (setwritedeadline scan, sudoers
// policy diff) that need an absolute path to the project root regardless
// of where `go test` is invoked from.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller unavailable")
	}
	dir := filepath.Dir(thisFile)
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("go.mod not found walking up from %s", thisFile)
	return ""
}
