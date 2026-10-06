package cron

import (
	"os"
	"testing"

	"github.com/naozhi/naozhi/internal/leakcheck"
	"github.com/naozhi/naozhi/internal/osutil"
)

// TestMain gives the package a goroutine-leak baseline: fail mode, so a test
// that leaves a goroutine behind reddens the package instead of printing a
// warning nobody reads. It also turns fsync off: nearly every job mutation
// and run append is a WriteFileAtomic, and the package spent most of its wall
// time blocked in fsync. Crash durability is covered by osutil's own tests.
func TestMain(m *testing.M) {
	osutil.DisableFsyncForTesting()
	os.Exit(leakcheck.Main(m, false))
}
