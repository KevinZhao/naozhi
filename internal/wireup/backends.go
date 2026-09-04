// backends.go holds the explicit (not init()-driven) backend.RegisterDefaults
// call so a missing import fails loudly at boot (docs/rfc/multi-backend.md §3).
package wireup

import (
	"sync"

	"github.com/naozhi/naozhi/internal/cli/backend"
)

// registerOnce guards backend.RegisterDefaults; package-level so tests can
// reset it between runs.
var (
	registerOnce sync.Once
	registered   bool
)

// RegisterCLIBackends invokes backend.RegisterDefaults exactly once and
// reports whether registration has run. The underlying registry panics on
// duplicate IDs, so the once-guard makes repeat calls safe.
func RegisterCLIBackends() bool {
	registerOnce.Do(func() {
		backend.RegisterDefaults()
		registered = true
		recordBootStep("cli-backends", BootStep{
			Kind:   "cli-backends",
			Detail: "backend.RegisterDefaults (claude+kiro profiles)",
		})
	})
	return registered
}

// EnsureCLIBackends invokes RegisterCLIBackends and discards the result.
func EnsureCLIBackends() {
	_ = RegisterCLIBackends()
}
