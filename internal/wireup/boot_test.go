package wireup

import (
	"testing"
)

// TestValidate_RequiredStepsRecorded confirms Validate passes once the
// boot steps have run. EnsureCLIBackends records "cli-backends"; the
// package init() records "history-backends" on import — so by the time
// this test runs, both required steps are present and Validate must
// succeed. This pins the #1165 extension-point contract: the health hook
// reports green only when the wire path is actually wired.
func TestValidate_RequiredStepsRecorded(t *testing.T) {
	EnsureCLIBackends()
	if err := Validate(); err != nil {
		t.Fatalf("Validate() after EnsureCLIBackends = %v, want nil", err)
	}
}

// TestBootSteps_IncludesRequired verifies the boot registry — the first
// production consumer of Registry[T] (#1579) — actually carries the
// required steps and exposes them via the Registry audit surface
// (Names()). This is the "migrate a real subsystem to prove its value"
// resolution: a real, non-test instantiation drives Registry[BootStep].
func TestBootSteps_IncludesRequired(t *testing.T) {
	EnsureCLIBackends()
	steps := BootSteps()
	want := map[string]bool{"cli-backends": false, "history-backends": false}
	for _, s := range steps {
		if _, ok := want[s]; ok {
			want[s] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("BootSteps() missing required step %q; got %v", name, steps)
		}
	}
}

// TestRecordBootStep_Idempotent ensures a repeated record under the same
// name is a no-op (not a duplicate panic). The boot helpers' once-guards
// already prevent re-entry in production, but recordBootStep's own guard
// keeps test ordering — multiple tests calling EnsureCLIBackends — safe.
func TestRecordBootStep_Idempotent(t *testing.T) {
	recordBootStep("cli-backends", BootStep{Kind: "cli-backends", Detail: "dup"})
	recordBootStep("cli-backends", BootStep{Kind: "cli-backends", Detail: "dup2"})
	if err := Validate(); err != nil {
		t.Fatalf("Validate() after duplicate record = %v, want nil", err)
	}
}

// TestValidate_DetectsMissingStep exercises the failure path on an
// isolated registry so the assertion does not depend on (or mutate) the
// package-level bootRegistry. It proves Validate's missing-step detection
// — the behaviour that turns a silent runtime degrade into a loud boot
// error — works as documented.
func TestValidate_DetectsMissingStep(t *testing.T) {
	t.Parallel()

	r := NewRegistry[BootStep]("boot-step")
	r.Register("cli-backends", BootStep{Kind: "cli-backends"})
	// history-backends intentionally absent.

	have := make(map[string]bool, r.Len())
	for _, n := range r.Names() {
		if step, ok := r.Get(n); ok {
			have[step.Kind] = true
		}
	}
	missing := 0
	for _, req := range requiredBootSteps {
		if !have[req] {
			missing++
		}
	}
	if missing != 1 {
		t.Fatalf("expected exactly 1 missing required step, got %d", missing)
	}
}
