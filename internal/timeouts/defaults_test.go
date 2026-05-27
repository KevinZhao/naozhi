package timeouts

import (
	"testing"
	"time"
)

// TestGetReturnsCopy verifies that mutating the returned struct does not
// affect subsequent calls — protects callers that mistakenly treat the
// result as a shared pointer.
func TestGetReturnsCopy(t *testing.T) {
	t.Parallel()
	a := Get()
	a.CLIClose = 0
	b := Get()
	if b.CLIClose == 0 {
		t.Fatalf("Get() must return a struct copy: mutating one snapshot leaked into the next call")
	}
}

// TestDefaultsArePopulated catches a future refactor that accidentally
// zero-values one of the fields. The field list is enumerated explicitly
// so adding a field forces the test to be updated alongside.
func TestDefaultsArePopulated(t *testing.T) {
	t.Parallel()
	d := Get()
	checks := []struct {
		name string
		v    time.Duration
	}{
		{"HTTPIdle", d.HTTPIdle},
		{"HTTPRead", d.HTTPRead},
		{"HTTPShutdown", d.HTTPShutdown},
		{"CLIClose", d.CLIClose},
		{"CLIInterrupt", d.CLIInterrupt},
		{"SessionReboot", d.SessionReboot},
	}
	for _, c := range checks {
		if c.v <= 0 {
			t.Errorf("Defaults.%s = %v; want > 0 (zero-valued field would silently break the consuming code path)", c.name, c.v)
		}
	}
}

// TestOverrideRestoresOnCleanup is the load-bearing contract for tests
// that want to flip a timeout for one test only. Without the t.Cleanup
// the override would leak into subsequent tests in the same package and
// produce flaky-only-when-running-the-whole-suite failures.
func TestOverrideRestoresOnCleanup(t *testing.T) {
	prior := Get().CLIClose
	if prior == 0 {
		t.Fatalf("precondition: prior CLIClose must be non-zero")
	}
	t.Run("inside-override", func(t *testing.T) {
		Override(t, func(d *Defaults) { d.CLIClose = 1 * time.Millisecond })
		if got := Get().CLIClose; got != 1*time.Millisecond {
			t.Fatalf("Override did not apply: got %v want 1ms", got)
		}
	})
	if got := Get().CLIClose; got != prior {
		t.Fatalf("Override did not restore on cleanup: got %v want %v", got, prior)
	}
}

// TestOverrideNilSetIsNoop documents that a nil callback is a safe
// "snapshot only" pattern — useful for tests that just want to assert
// the canonical values without registering a cleanup.
func TestOverrideNilSetIsNoop(t *testing.T) {
	t.Parallel()
	prior := Get()
	got := Override(t, nil)
	if got != prior {
		t.Fatalf("Override(t, nil) should return the canonical snapshot unchanged")
	}
}
