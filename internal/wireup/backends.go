// Package wireup also owns the explicit construction-time registrations
// that previously scattered across cmd/naozhi/main.go. Sprint 1c of the
// ARCH-B refactor (#793 / R246-ARCH-19): pull register-style calls out
// of main.go so the binary entry point reads as a graph of explicit
// constructors and the registration sequence has one inspectable owner.
//
// The history-backend blank-imports remain in history_backends.go; this
// file holds the *explicit* RegisterDefaults() calls that are intentionally
// not driven by init() — explicit, not init()-driven, so missing imports
// or argument-order regressions fail loudly at boot rather than at the
// first runtime use. docs/rfc/multi-backend.md §3.
package wireup

import (
	"sync"

	"github.com/naozhi/naozhi/internal/cli/backend"
)

// registerOnce guards the once-only invocation of backend.RegisterDefaults.
// Defined as a package-private var (not a sync.Once literal at the call
// site) so test helpers in wireup_test.go can reset the guard between
// tests — production code only ever calls RegisterCLIBackends, which
// drives the Once exactly once per process lifetime.
var (
	registerOnce sync.Once
	registered   bool
)

// RegisterCLIBackends invokes backend.RegisterDefaults exactly once,
// returning a flag indicating whether registration ran. Idempotent:
// the underlying registry is monotonically additive and panics on
// duplicate IDs, so repeat calls would surface accidental double-init —
// the once-guard here lets test setup ordering treat RegisterCLIBackends
// as a fire-and-forget call without needing a sync.Once at every call
// site.
//
// Why pull this into wireup vs. leaving it in main.go: the registration
// must happen before any consumer (discovery, server, dispatch) looks up
// DisplayName / DefaultTag / DetectInProc by ID. Co-locating the call
// alongside the history-backend blank-imports gives operators a single
// place to audit "what runs on the wire path" and matches the issue's
// proposal to extend wireup ownership beyond a single 27-line file.
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

// EnsureCLIBackends invokes RegisterCLIBackends and discards the
// already-registered signal. Lighter-weight call site for code paths
// that just need the registry populated and don't care whether they
// triggered the registration themselves.
func EnsureCLIBackends() {
	_ = RegisterCLIBackends()
}
