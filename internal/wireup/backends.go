// backends.go holds the explicit (not init()-driven) backend.RegisterDefaults
// call so a missing import fails loudly at boot (docs/rfc/multi-backend.md §3).
package wireup

import (
	"sync"

	"github.com/naozhi/naozhi/internal/cli/backend"
)

// registerOnce guards backend.RegisterDefaults: the underlying registry panics
// on duplicate IDs, and tests call RegisterCLIBackends more than once.
var registerOnce sync.Once

// RegisterCLIBackends invokes backend.RegisterDefaults exactly once per
// process. Repeat calls are no-ops.
func RegisterCLIBackends() {
	registerOnce.Do(backend.RegisterDefaults)
}
