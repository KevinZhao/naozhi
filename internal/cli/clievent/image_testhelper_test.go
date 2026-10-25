package clievent

import (
	"os"
	"path/filepath"
	"testing"
)

// imageSafeTempDir returns a temp dir under /tmp so that ExtractImagePaths'
// safeImageDirs allowlist accepts fixture files. The path is the literal
// /tmp/... one; on macOS it resolves to /private/tmp/..., see resolvedPath.
func imageSafeTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "naozhi-img-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// resolvedPath is path with symlinks resolved, as ExtractImagePaths reports
// it in ImagePath.Path.
func resolvedPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", path, err)
	}
	return resolved
}
