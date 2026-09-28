package server

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/leakcheck"
)

// TestMain gives the package a goroutine-leak baseline: fail mode, so a test
// that leaves a goroutine behind reddens the package instead of printing a
// warning nobody reads.
//
// It also points HOME and CLAUDE_PROJECTS_DIR at an empty directory. Every
// Server a test builds warms the history cache from the claude directory
// under HOME; on a developer machine that read the operator's real sessions
// and scanned the filesystem for their workspaces, so the package depended on
// the host and a long scan outlived the tests into the leak check. Tests that
// need their own home still t.Setenv it.
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	home, err := os.MkdirTemp("", "naozhi-server-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "server tests: temp HOME:", err)
		return 1
	}
	defer os.RemoveAll(home)
	os.Setenv("HOME", home)
	os.Setenv("CLAUDE_PROJECTS_DIR", filepath.Join(home, ".claude", "projects"))
	return leakcheck.Main(m, false)
}
