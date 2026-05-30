package session

import (
	"os"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/backend"
)

// TestMain pre-populates the backend registry once for the whole session test
// binary. R239-ARCH-D: costUnitForBackend (and other session read paths) now
// assume the registry is already populated rather than lazily mutating the
// process-global from a query helper. Production wires
// backend.RegisterDefaults() in cmd/naozhi/main.go before any session is
// constructed; tests get the equivalent here. EnsureDefaults is idempotent and
// concurrent-safe, so it cooperates with any sibling package that already
// registered the built-ins.
func TestMain(m *testing.M) {
	backend.EnsureDefaults()
	os.Exit(m.Run())
}
