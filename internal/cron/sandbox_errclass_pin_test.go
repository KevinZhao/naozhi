package cron

import (
	"testing"

	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// TestSandboxErrClass_WireEquivalence pins the cron-local sandbox error
// classes to their runtelemetry twins. The two packages deliberately
// declare the strings separately (cron's local ErrorClass convention);
// a rename on either side that breaks the wire equivalence breaks
// dashboard badge logic keyed on the literal — fail here first.
func TestSandboxErrClass_WireEquivalence(t *testing.T) {
	pairs := map[ErrorClass]runtelemetry.ErrorClass{
		ErrClassSandboxFailed:      runtelemetry.ErrClassCronSandboxFailed,
		ErrClassSandboxTransport:   runtelemetry.ErrClassCronSandboxTransport,
		ErrClassSandboxUnavailable: runtelemetry.ErrClassCronSandboxUnavailable,
	}
	for c, r := range pairs {
		if string(c) != string(r) {
			t.Errorf("cron %q != runtelemetry %q", string(c), string(r))
		}
	}
}
