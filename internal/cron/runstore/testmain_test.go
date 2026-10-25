package runstore

import (
	"os"
	"testing"

	"github.com/naozhi/naozhi/internal/osutil"
)

// TestMain turns fsync off because nearly every test appends records through
// WriteFileAtomic. Crash durability is covered by osutil's own tests.
func TestMain(m *testing.M) {
	osutil.DisableFsyncForTesting()
	os.Exit(m.Run())
}
