// Package wireup — boot.go is the single inspectable owner of the
// process-level boot-time registration set, and the first production
// consumer of Registry[T].
//
// R20260602-ARCH-2 (#1579): Registry[T] was introduced (registry.go) as
// the unified registration idiom but had zero production callers — only
// registry_test.go referenced it, making it a tested-but-unused generic.
// boot.go migrates a real subsystem onto it (issue option 2: "迁移一个真实
// 子系统证明其价值") by recording each boot-time wireup step in a
// Registry[BootStep]. The audit list (Registry.Names) and duplicate-guard
// now back a live feature instead of a speculative API.
//
// R250-ARCH-2 (#1165): history_backends.go promised "a single explicit
// place for init()-side wireup" but the promise was half-applied — there
// was no extension point and no Validate() hook, and cmd/naozhi/main.go
// still called backend.RegisterDefaults() directly (main.go:116) instead
// of routing through wireup.RegisterCLIBackends(). boot.go closes that gap:
//
//   - RegisterCLIBackends/EnsureCLIBackends now also record a BootStep so
//     the registry reflects what actually ran on the wire path.
//   - Validate() is the missing extension-point/health hook: callers run
//     it after wireup to confirm the required boot steps executed, turning
//     a dropped registration into a loud boot error instead of a silent
//     runtime degrade (the exact failure mode R249-ARCH-9 chased).
package wireup

import (
	"fmt"
	"sort"
	"sync"
)

// BootStep describes one boot-time wireup step that ran in this process.
// It is the value type stored in the bootRegistry — a real (non-test)
// instantiation of Registry[T], which is the point of #1579's option 2.
type BootStep struct {
	// Kind groups the step ("cli-backends", "history-backends",
	// "schedulers") so Validate can assert a category ran without
	// pinning an exact step name.
	Kind string
	// Detail is a short human note for audit/log output (e.g. which
	// profiles or backends the step wired).
	Detail string
}

// bootRegistry is the live, production consumer of Registry[T]. Every
// boot-time wireup helper records its step here so operators have a single
// "what got wired" surface (bootRegistry.Names()) and a double-wireup of
// the same step panics loudly at boot rather than silently shadowing.
//
// The Registry[T] value is internally concurrency-safe, but the package-level
// pointer itself is swapped by tests (TestValidate_DetectsMissingStep /
// TestValidate_KindDoesNotSatisfyName via setBootRegistry). bootRegistryMu
// guards that pointer so concurrent (e.g. -parallel) readers never race the
// swap; all access goes through getBootRegistry / setBootRegistry (#1611).
var (
	bootRegistryMu sync.RWMutex
	bootRegistry   = NewRegistry[BootStep]("boot-step")
)

// getBootRegistry returns the current boot registry under a read lock so a
// concurrent setBootRegistry swap cannot race the pointer load.
func getBootRegistry() *Registry[BootStep] {
	bootRegistryMu.RLock()
	defer bootRegistryMu.RUnlock()
	return bootRegistry
}

// setBootRegistry swaps the boot registry pointer under a write lock. It is
// the single mutation point (used by tests to inject a fixture registry and
// restore the original) so the swap is synchronized with all readers.
func setBootRegistry(r *Registry[BootStep]) {
	bootRegistryMu.Lock()
	defer bootRegistryMu.Unlock()
	bootRegistry = r
}

// init records the history-backends step. The blank imports in
// history_backends.go are compiled into this package unconditionally, so
// merely importing wireup (which cmd/naozhi does) guarantees the history
// factories' init() blocks ran — recording the step here keeps
// history_backends.go free of exported symbols (its godoc invariant) while
// still surfacing the step in the boot audit + Validate set.
func init() {
	recordBootStep("history-backends", BootStep{
		Kind:   "history-backends",
		Detail: "claudejsonl + kirojsonl history factories",
	})
}

// recordBootStep adds a step to the boot registry. It is idempotent at the
// call-site level via the once-guards in the individual wireup helpers;
// the underlying Registry panics on a true duplicate, which would mean two
// independent helpers claimed the same step name — a wiring bug.
func recordBootStep(name string, step BootStep) {
	reg := getBootRegistry()
	if _, already := reg.Get(name); already {
		return
	}
	reg.Register(name, step)
}

// BootSteps returns the names of every boot step recorded so far, sorted
// for deterministic audit output. Exposes the Registry[T] audit surface to
// callers (cmd/naozhi can log it at startup).
func BootSteps() []string { return getBootRegistry().Names() }

// requiredBootSteps are the wireup steps that MUST have run before naozhi
// serves traffic. Validate checks these so a dropped registration fails at
// boot, not at the first runtime lookup. history-backends is satisfied by
// the blank imports in history_backends.go (recorded via init()).
var requiredBootSteps = []string{"cli-backends", "history-backends"}

// Validate reports an error if any required boot step did not run. It is
// the extension-point/health hook #1165 found missing: cmd/naozhi calls it
// after the wireup steps so a missing import or a no-op'd helper aborts
// startup with a clear message instead of degrading silently.
func Validate() error {
	reg := getBootRegistry()
	var missing []string
	for _, req := range requiredBootSteps {
		if _, ok := reg.Get(req); !ok {
			missing = append(missing, req)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("wireup: required boot steps did not run: %v", missing)
	}
	return nil
}
