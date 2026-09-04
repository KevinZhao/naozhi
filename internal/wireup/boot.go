// boot.go is the inspectable owner of the boot-time registration set: each
// wireup step records a BootStep in a Registry, and Validate() turns a
// dropped registration into a loud boot error instead of a silent runtime
// degrade (#1165, #1579).
package wireup

import (
	"fmt"
	"sort"
	"sync"
)

// BootStep describes one boot-time wireup step that ran in this process.
type BootStep struct {
	// Kind groups the step ("cli-backends", "history-backends", "schedulers").
	Kind string
	// Detail is a short human note for audit/log output.
	Detail string
}

// bootRegistry is the "what got wired" surface. The Registry is internally
// concurrency-safe, but tests swap the pointer itself, so bootRegistryMu guards
// it and all access goes through getBootRegistry / setBootRegistry (#1611).
var (
	bootRegistryMu sync.RWMutex
	bootRegistry   = NewRegistry[BootStep]("boot-step")
)

// getBootRegistry returns the current boot registry under a read lock.
func getBootRegistry() *Registry[BootStep] {
	bootRegistryMu.RLock()
	defer bootRegistryMu.RUnlock()
	return bootRegistry
}

// setBootRegistry swaps the boot registry pointer (tests inject a fixture).
func setBootRegistry(r *Registry[BootStep]) {
	bootRegistryMu.Lock()
	defer bootRegistryMu.Unlock()
	bootRegistry = r
}

// init records the history-backends step: importing wireup guarantees the
// blank-imported history factories' init() blocks ran.
func init() {
	recordBootStep("history-backends", BootStep{
		Kind:   "history-backends",
		Detail: "claudejsonl + kirojsonl history factories",
	})
}

// recordBootStep adds a step to the boot registry; an already-recorded name
// is a no-op.
func recordBootStep(name string, step BootStep) {
	reg := getBootRegistry()
	if _, already := reg.Get(name); already {
		return
	}
	reg.Register(name, step)
}

// BootSteps returns the names of every boot step recorded so far, sorted.
func BootSteps() []string { return getBootRegistry().Names() }

// requiredBootSteps MUST have run before naozhi serves traffic.
var requiredBootSteps = []string{"cli-backends", "history-backends"}

// Validate reports an error if any required boot step did not run; cmd/naozhi
// calls it after wireup so a missing import aborts startup with a clear message.
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
