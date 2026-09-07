// backends.go holds the explicit (not init()-driven) backend.RegisterDefaults
// call so a missing import fails loudly at boot (docs/rfc/multi-backend.md §3).
package wireup

import (
	"sync"

	"github.com/naozhi/naozhi/internal/cli/backend"
)

// registerOnce guards backend.RegisterDefaults. Genuinely process-global, not a
// convenience: the underlying registry panics on duplicate IDs, so registration
// can happen at most once per process no matter how many Boot values exist.
// Boot records that it happened; it does not own the fact.
var (
	registerOnce sync.Once
	registered   bool
)

// RegisterCLIBackends invokes backend.RegisterDefaults exactly once and
// reports whether registration has run. Repeat calls are safe (see registerOnce);
// the boot step is recorded on the Boot that first triggers it.
func (b *Boot) RegisterCLIBackends() bool {
	registerOnce.Do(func() {
		backend.RegisterDefaults()
		registered = true
	})
	// Recorded outside the once so a second Boot in the same process (tests)
	// still observes the step rather than reporting a half-wired process.
	if registered {
		b.recordStep("cli-backends", BootStep{
			Kind:   "cli-backends",
			Detail: "backend.RegisterDefaults (claude+kiro profiles)",
		})
	}
	return registered
}

// EnsureCLIBackends invokes RegisterCLIBackends and discards the result.
func (b *Boot) EnsureCLIBackends() {
	_ = b.RegisterCLIBackends()
}
