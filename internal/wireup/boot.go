// boot.go owns the boot-time registration set: each wireup step records a
// BootStep, and Validate turns a dropped registration into a loud boot error
// instead of a silent runtime degrade (#1165, #1579).
//
// The record used to live in a swappable package-level var, and the
// history-backends step was recorded from an init(). Both are gone (#2552): a
// Boot value is created by the caller and threaded, so boot order is explicit
// and tests get their own instance instead of saving, swapping and restoring
// process-global state under a mutex.
package wireup

import (
	"fmt"
	"sort"
)

// BootStep describes one boot-time wireup step that ran in this process.
type BootStep struct {
	// Kind groups the step ("cli-backends", "history-backends", "schedulers").
	Kind string
	// Detail is a short human note for audit/log output.
	Detail string
}

// Boot is the wireup composition root: it performs the boot-time wireup steps
// and records what ran so Validate can refuse to serve on a half-wired process.
// cmd/naozhi creates exactly one; tests create their own.
//
// Note that some of what it records is unavoidably process-global —
// backend.RegisterDefaults panics on duplicate IDs and the history factories
// register from blank-import init() blocks in other packages. Boot does not
// pretend otherwise; it makes the OBSERVATION of those facts explicit and
// per-instance, which is what the old swappable global got wrong.
type Boot struct {
	steps *Registry[BootStep]
}

// NewBoot returns an empty Boot. Call the Record*/Ensure* steps, then Validate.
func NewBoot() *Boot {
	return &Boot{steps: NewRegistry[BootStep]("boot-step")}
}

// RecordHistoryBackends records that the blank-imported history factories are
// linked in. This was an init() until #2552: importing wireup "proved" the
// factories' own init() blocks had run, which made the guarantee invisible at
// the call site and impossible to assert against. Now main calls it, and a
// caller that forgets fails loudly in Validate — the same fail-loud outcome
// #1165 wanted from EnsureCLIBackends.
func (b *Boot) RecordHistoryBackends() {
	b.recordStep("history-backends", BootStep{
		Kind:   "history-backends",
		Detail: "claudejsonl + kirojsonl history factories",
	})
}

// recordStep adds a step; an already-recorded name is a no-op.
func (b *Boot) recordStep(name string, step BootStep) {
	if _, already := b.steps.Get(name); already {
		return
	}
	b.steps.Register(name, step)
}

// Steps returns the names of every boot step recorded so far, sorted.
func (b *Boot) Steps() []string { return b.steps.Names() }

// requiredBootSteps MUST have run before naozhi serves traffic.
var requiredBootSteps = []string{"cli-backends", "history-backends"}

// Validate reports an error if any required boot step did not run; cmd/naozhi
// calls it after wireup so a missing import aborts startup with a clear message.
func (b *Boot) Validate() error {
	var missing []string
	for _, req := range requiredBootSteps {
		if _, ok := b.steps.Get(req); !ok {
			missing = append(missing, req)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("wireup: required boot steps did not run: %v", missing)
	}
	return nil
}
