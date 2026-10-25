package server

import (
	"testing"
	"time"
)

// TestHealthProbe_TypeIsClosure pins the HealthProbe type as a func
// (not an interface). Closure-as-probe is load-bearing because the
// migration plan calls for tests to register ad-hoc probes via
// `RegisterProbe(func(...) {...})` — switching to an interface would
// silently force every test to define a named probe type.
func TestHealthProbe_TypeIsClosure(t *testing.T) {
	// If HealthProbe is ever changed to an interface, this assignment
	// from a literal func breaks at compile time. The presence of this
	// test in CI guarantees no silent reshape.
	var p HealthProbe = func(auth *healthAuthSection) {}
	if p == nil {
		t.Fatal("probe assignment from func literal must yield a non-nil HealthProbe")
	}
	auth := &healthAuthSection{}
	p(auth) // call must not panic
}

// TestWSDroppedHealthProbe_SetsCounter verifies the ws_dropped probe
// copies the injected closure's value into a fresh heap int64 and points
// WSDropped at it (the wire field is a *int64 so omitempty drops it when
// no hub is wired). #1052.
func TestWSDroppedHealthProbe_SetsCounter(t *testing.T) {
	auth := &healthAuthSection{}
	wsDroppedHealthProbe(func() int64 { return 7 })(auth)
	if auth.WSDropped == nil {
		t.Fatal("WSDropped must be non-nil after probe with live closure")
	}
	if *auth.WSDropped != 7 {
		t.Errorf("WSDropped = %d, want 7", *auth.WSDropped)
	}
}

// TestDispatchHealthProbe_PopulatesAndOmitsLastReply pins both arms of
// the dispatch probe: counters always copy through, and the
// last-reply-success fields are emitted only when the timestamp is
// non-zero (byte-identical to the prior inline `if !lastReply.IsZero()`
// guard). #1052.
func TestDispatchHealthProbe_PopulatesAndOmitsLastReply(t *testing.T) {
	// Zero lastReply → counters set, timestamp fields stay empty.
	auth := &healthAuthSection{}
	dispatchHealthProbe(func() (int64, int64, int64, time.Time) {
		return 3, 1, 2, time.Time{}
	})(auth)
	if auth.Dispatch == nil {
		t.Fatal("Dispatch must be non-nil after probe with live closure")
	}
	if auth.Dispatch.MessageCount != 3 || auth.Dispatch.ReplyErrorCount != 1 || auth.Dispatch.SendFailCount != 2 {
		t.Errorf("dispatch counters = %+v, want {3,1,2}", auth.Dispatch)
	}
	if auth.Dispatch.LastReplySuccessAt != "" || auth.Dispatch.LastReplySuccessAgo != "" {
		t.Errorf("last-reply fields must stay empty for zero timestamp, got at=%q ago=%q",
			auth.Dispatch.LastReplySuccessAt, auth.Dispatch.LastReplySuccessAgo)
	}

	// Non-zero lastReply → RFC3339 + ago populated.
	auth2 := &healthAuthSection{}
	dispatchHealthProbe(func() (int64, int64, int64, time.Time) {
		return 0, 0, 0, time.Now().Add(-time.Minute)
	})(auth2)
	if auth2.Dispatch.LastReplySuccessAt == "" || auth2.Dispatch.LastReplySuccessAgo == "" {
		t.Errorf("last-reply fields must be set for non-zero timestamp, got at=%q ago=%q",
			auth2.Dispatch.LastReplySuccessAt, auth2.Dispatch.LastReplySuccessAgo)
	}
}
