package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageHomeIsIsolated: a Server built by any test resolves its claude
// directories inside the package's temporary home, never the host's.
func TestPackageHomeIsIsolated(t *testing.T) {
	home := os.Getenv("HOME")
	if !strings.Contains(filepath.Base(home), "naozhi-server-test-home-") {
		t.Fatalf("HOME = %q, want the package's temporary home", home)
	}
	if got := resolveClaudeDir(); !strings.HasPrefix(got, home) {
		t.Errorf("resolveClaudeDir() = %q, want it under %q", got, home)
	}
	if got := resolveClaudeProjectsDir(); !strings.HasPrefix(got, home) {
		t.Errorf("resolveClaudeProjectsDir() = %q, want it under %q", got, home)
	}
}
