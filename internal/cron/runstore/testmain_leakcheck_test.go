package runstore

import (
	"os"
	"testing"

	"github.com/naozhi/naozhi/internal/leakcheck"
	"github.com/naozhi/naozhi/internal/osutil"
)

// TestMain keeps the baseline these tests had while they lived in package
// cron: a goroutine leak fails the package, and fsync is off because nearly
// every test appends records through WriteFileAtomic. Crash durability is
// covered by osutil's own tests.
func TestMain(m *testing.M) {
	osutil.DisableFsyncForTesting()
	os.Exit(leakcheck.Main(m, false))
}
