package server

import (
	"os"
	"testing"

	"github.com/naozhi/naozhi/internal/leakcheck"
)

// TestMain gives the package a goroutine-leak baseline: fail mode, so a test
// that leaves a goroutine behind reddens the package instead of printing a
// warning nobody reads.
func TestMain(m *testing.M) {
	os.Exit(leakcheck.Main(m, false))
}
