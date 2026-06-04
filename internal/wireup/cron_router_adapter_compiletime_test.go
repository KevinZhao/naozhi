package wireup

import "testing"

// TestCompileTimeOrdinalPins_CompilesOK is a build-only sentinel for
// R20260604-ARCH-8. The package-level const block in cron_router_adapter.go
// uses uint(int(cron.X)-int(session.X)) expressions whose compilation IS the
// ordinal assertion — a diverging iota causes a negative-to-uint overflow and
// a compile error before any binary exists. This test exists to document that
// intent: if the package builds, the pins passed.
func TestCompileTimeOrdinalPins_CompilesOK(t *testing.T) {
	t.Parallel()
	// No runtime work needed. Compilation of this package is the assertion.
}
