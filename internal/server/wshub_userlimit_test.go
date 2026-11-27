package server

import "testing"

// TestHubAdmitSend_UsesOwnerBudget pins R244-SEC-P2-3 / #888: the
// per-uploadOwner send-budget bucket caps the aggregate at burst=5 however
// many WS tabs the same user holds open, and two owners do not share a bucket.
func TestHubAdmitSend_UsesOwnerBudget(t *testing.T) {
	h := &Hub{admit: newConnAdmission(HubOptions{})}
	allowed := 0
	for i := 0; i < 50; i++ {
		if h.admitSend("owner-A") {
			allowed++
		}
	}
	// Slack on the upper bound covers a token refill before the loop ends;
	// the failure this pins is an unbounded admit, not exact arithmetic.
	if allowed < 5 || allowed > 7 {
		t.Errorf("expected ~5 admits per owner (burst=5), got %d", allowed)
	}
	if !h.admitSend("owner-B") {
		t.Error("another owner was throttled by owner-A's budget")
	}
}

// TestAllowSendForOwner_EmptyOwnerSkipsGate pins that anonymous
// (uploadOwner == "") sends bypass the per-user ceiling — the no-token
// single-user path uses an empty owner key before the nz_anon cookie
// mints, and a hard refusal there would brick legitimate first-message
// flows. The per-conn sendLimiter is the only gate in that path.
func TestAllowSendForOwner_EmptyOwnerSkipsGate(t *testing.T) {
	h := newConnAdmission(HubOptions{})
	for i := 0; i < 100; i++ {
		if !h.allowSend("") {
			t.Fatalf("empty owner should not be throttled (call %d)", i)
		}
	}
}
