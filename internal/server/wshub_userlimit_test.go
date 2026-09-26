package server

import (
	"sync"
	"testing"
)

// TestAllowSendForOwner_NTabsShareBudget pins R244-SEC-P2-3 / #888: the
// per-uploadOwner send-budget bucket caps the aggregate at the same
// burst=5 limit regardless of how many WS tabs (== how many wsClient
// instances each with their own per-conn sendLimiter) the same user
// holds open. The previous shape allowed N tabs × 5 burst = 5N which
// let one logged-in user fan out per-message work N× past the
// single-tab ceiling.
func TestAllowSendForOwner_NTabsShareBudget(t *testing.T) {
	h := newConnAdmission(HubOptions{})

	const owner = "owner-A"
	allowed := 0
	for i := 0; i < 50; i++ {
		if h.allowSend(owner) {
			allowed++
		}
	}
	// burst is 5, so the first 5 should pass and the rest should be
	// throttled (the rate.Every(time.Second) sustained refill is far
	// slower than this tight loop). Allow some slack on the upper bound
	// to cover the edge case where the limiter records a non-zero token
	// refill before the loop completes; the failure mode this test pins
	// is the *unbounded* per-call admit, not exact burst arithmetic.
	if allowed < 5 || allowed > 7 {
		t.Errorf("expected ~5 admits per owner (burst=5), got %d", allowed)
	}
}

// TestAllowSendForOwner_OwnersIndependent pins that two distinct users
// do NOT share a bucket — bucketing by uploadOwner means user-B's
// throttle does not depend on user-A's recent traffic.
func TestAllowSendForOwner_OwnersIndependent(t *testing.T) {
	h := newConnAdmission(HubOptions{})

	// Drain owner A.
	for i := 0; i < 10; i++ {
		h.allowSend("A")
	}
	// Owner B should still get its full burst.
	allowedB := 0
	for i := 0; i < 10; i++ {
		if h.allowSend("B") {
			allowedB++
		}
	}
	if allowedB < 5 {
		t.Errorf("owner B should retain its independent budget; got %d admits", allowedB)
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

// TestAllowSendForOwner_ConcurrentAccess pins that the lock guards both
// the lookup-or-create and the limiter Allow() against concurrent
// callers from N tabs hitting the WS readPump in parallel. Race
// detection is enabled by `go test -race`.
func TestAllowSendForOwner_ConcurrentAccess(t *testing.T) {
	h := newConnAdmission(HubOptions{})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				h.allowSend("shared-owner")
			}
		}()
	}
	wg.Wait()
}
