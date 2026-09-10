package wireup

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/backend"
)

// TestRegisterCLIBackends verifies the wireup-side helper actually
// populates the cli/backend registry. The helper is the migration
// target for cmd/naozhi/main.go's existing backend.RegisterDefaults
// call (#793); the test pins the contract so the helper cannot
// silently degrade to a no-op while consumers still depend on the
// claude/kiro profiles being present.
func TestRegisterCLIBackends(t *testing.T) {
	// Drive the helper directly. Note: backend.RegisterDefaults is
	// idempotent at the registry layer (panics on duplicate IDs) and
	// the wireup-level sync.Once is a belt-and-braces guard, so a
	// second call inside the same process — including from another
	// test that already imported wireup transitively — is a no-op.
	NewBoot().EnsureCLIBackends()

	if _, ok := backend.Get("claude"); !ok {
		t.Fatal("RegisterCLIBackends did not register the claude profile")
	}
	if _, ok := backend.Get("kiro"); !ok {
		t.Fatal("RegisterCLIBackends did not register the kiro profile")
	}
}

// TestRegisterCLIBackendsIdempotent verifies repeated calls do not
// re-invoke backend.RegisterDefaults (which would panic on duplicate
// registration). The once-guard inside wireup is the migration safety
// net — if the helper is called from main.go AND a test setup, the
// process must still boot.
func TestRegisterCLIBackendsIdempotent(t *testing.T) {
	// Three calls across two Boot values; if the once-guard is broken the
	// second would panic via backend.Register's duplicate-ID check. Two Boots
	// on purpose: registration is process-global (#2552), so a fresh Boot must
	// still observe it rather than re-running it.
	NewBoot().EnsureCLIBackends()
	b := NewBoot()
	b.EnsureCLIBackends()
	if !b.RegisterCLIBackends() {
		t.Fatal("RegisterCLIBackends should report registered=true after first call")
	}
}

// TestRegisterCLIBackends_RecordsBootStep pins the #1165 "single explicit
// place for init()-side wireup" promise that used to be half-applied:
// driving the CLI-backend registration through wireup must now leave an
// audit trail in the boot registry, and Validate must treat the cli-backends
// step as satisfied. This is what lets cmd/naozhi route through
// boot.EnsureCLIBackends + boot.Validate instead of calling
// backend.RegisterDefaults directly and hoping it ran.
func TestRegisterCLIBackends_RecordsBootStep(t *testing.T) {
	b := NewBoot()
	b.EnsureCLIBackends()

	var seen bool
	for _, s := range b.Steps() {
		if s == "cli-backends" {
			seen = true
			break
		}
	}
	if !seen {
		t.Fatalf("EnsureCLIBackends did not record the cli-backends boot step; got %v", b.Steps())
	}
	// history-backends is recorded explicitly since #2552, so a Boot that only
	// ran the CLI step must NOT validate — that is the fail-loud property the
	// old init() hid by always recording it on import.
	if err := b.Validate(); err == nil {
		t.Error("Validate() = nil with only cli-backends recorded; history-backends must still be required")
	}
	b.RecordHistoryBackends()
	if err := b.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil after both required steps recorded", err)
	}
}
