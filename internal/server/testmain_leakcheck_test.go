package server

import (
	"os"
	"testing"

	"github.com/naozhi/naozhi/internal/leakcheck"
)

// TestMain gives the package a goroutine-leak baseline (#2537): warn mode
// while the instrumentation soaks — flip the flag to fail once the package
// has run clean for a week.
func TestMain(m *testing.M) {
	os.Exit(leakcheck.Main(m, true))
}
