package cron

import (
	"os"
	"testing"

	"github.com/naozhi/naozhi/internal/osutil"
)

// TestMain turns fsync off: nearly every job mutation and run append is a
// WriteFileAtomic, and the package spent most of its wall time blocked in
// fsync. Crash durability is covered by osutil's own tests.
func TestMain(m *testing.M) {
	osutil.DisableFsyncForTesting()
	os.Exit(m.Run())
}
