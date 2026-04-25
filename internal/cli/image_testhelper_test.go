package cli

import (
	"os"
	"runtime"
	"testing"
)

// imageSafeTempDir returns a temp dir under /tmp so that ExtractImagePaths'
// safeImageDirs allowlist ({"/tmp/"}) accepts fixture files.
//
// On macOS /tmp is itself a symlink to /private/tmp, and ExtractImagePaths
// calls filepath.EvalSymlinks on each candidate path — the resolved result
// therefore starts with /private/tmp/, which the allowlist does not match.
// This only affects local test runs; Linux production keeps /tmp as a real
// directory. Skip the affected suite on darwin rather than silently
// masking the mismatch.
func imageSafeTempDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("macOS /tmp is a symlink to /private/tmp; ExtractImagePaths " +
			"allowlist cannot match resolved paths here. Production is Linux.")
	}
	dir, err := os.MkdirTemp("/tmp", "naozhi-img-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
