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
	EnsureCLIBackends()

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
	// Three calls; if the once-guard is broken the second would
	// panic via backend.Register's duplicate-ID check.
	EnsureCLIBackends()
	EnsureCLIBackends()
	if !RegisterCLIBackends() {
		t.Fatal("RegisterCLIBackends should report registered=true after first call")
	}
}
