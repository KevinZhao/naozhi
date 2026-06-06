package server

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// installPlaceholderSub mirrors what handleSubscribe does just before it calls
// completeSubscribe: it installs the no-op placeholder unsub and (when caps are
// enforced) bumps the per-key subscriber counter + its lock-free mirror. Tests
// use it to drive completeSubscribe directly with the exact pre-state the real
// path produces.
func installPlaceholderSub(h *Hub, c *wsClient, key string) {
	h.mu.Lock()
	c.subscriptions[key] = func() {}
	if h.enforceCaps {
		h.subscriberCount[key]++
		h.setSubscriberCountFast(key, h.subscriberCount[key])
	}
	h.mu.Unlock()
}

// fastSubscriberCount reads the lock-free subscriberCountFast mirror for key.
func fastSubscriberCount(h *Hub, key string) int {
	if v, ok := h.subscriberCountFast.Load(key); ok {
		return int(v.(*atomic.Int32).Load())
	}
	return 0
}

func ownerSubState(h *Hub, c *wsClient, key string) (subCount int, hasSub bool, fast int) {
	h.mu.Lock()
	subCount = h.subscriberCount[key]
	_, hasSub = c.subscriptions[key]
	h.mu.Unlock()
	fast = fastSubscriberCount(h, key)
	return
}

// TestCompleteSubscribe_CtxCancelledInReCheckNoLeak is the R20260605B-CORR-2
// (#1806) regression guard. handleSubscribe installs a placeholder subscription
// and increments subscriberCount[key] before calling completeSubscribe. The bug
// lives specifically in the UNDER-LOCK ctx re-check branch (line ~262): when the
// hub ctx is cancelled in the window AFTER the pre-lock fast-fail passes but
// BEFORE the re-lock, that branch unlocked + unsub()'d + returned WITHOUT the
// delete + decrement its two sibling early returns perform, leaking the
// placeholder (toward maxSubscriptionsPerClient) and inflating
// subscriberCount[key] (toward maxSubscribersPerKey).
//
// To deterministically drive THAT branch (not the pre-lock fast-fail), the test
// holds h.mu so completeSubscribe blocks at its re-lock with the pre-lock
// fast-fail already passed (ctx still live there), then cancels the ctx before
// releasing h.mu. completeSubscribe therefore observes a live ctx at the
// fast-fail and a cancelled ctx at the re-check — exactly the #1806 window.
func TestCompleteSubscribe_CtxCancelledInReCheckNoLeak(t *testing.T) {
	hub, router := newTestHub("")
	defer hub.Shutdown()
	// newTestHub goes through NewHub, so enforceCaps is true and the counter
	// map is allocated — the caps this bug inflates are actually live.
	if !hub.enforceCaps {
		t.Fatal("expected enforceCaps=true via NewHub so the leak counters are active")
	}

	key := "test:d:u:general"
	proc := session.NewTestProcess()
	sess := router.InjectSession(key, proc)
	if !sess.HasProcess() {
		t.Fatal("injected session must have a live process to pass the HasProcess branch")
	}

	c := &wsClient{
		send:          make(chan []byte, 8),
		done:          make(chan struct{}),
		subscriptions: make(map[string]func()),
		subGen:        make(map[string]uint64),
	}
	installPlaceholderSub(hub, c, key)
	if cnt, ok, _ := ownerSubState(hub, c, key); cnt != 1 || !ok {
		t.Fatalf("pre-state: subscriberCount=%d hasSub=%v, want 1/true", cnt, ok)
	}

	// Hold h.mu so completeSubscribe parks at its under-lock re-check after the
	// pre-lock fast-fail (which sees a live ctx) and SubscribeEvents have run.
	hub.mu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.completeSubscribe(c, key, node.ClientMsg{Type: "subscribe", Key: key}, sess)
	}()
	// Give the goroutine time to clear the fast-fail + SubscribeEvents and block
	// on h.mu.Lock(). It cannot proceed past the re-lock until we Unlock below,
	// so this only needs to be long enough to reach the blocked Lock.
	time.Sleep(50 * time.Millisecond)
	// Cancel WITHOUT Shutdown — the ParentCtx-cancel-forgot-Shutdown scenario.
	hub.cancel()
	hub.mu.Unlock()
	<-done

	cnt, hasSub, fast := ownerSubState(hub, c, key)
	if hasSub {
		t.Errorf("placeholder subscription leaked after ctx-cancelled re-check branch")
	}
	if cnt != 0 {
		t.Errorf("subscriberCount[%q] = %d after ctx-cancelled re-check, want 0 (leak)", key, cnt)
	}
	if fast != 0 {
		t.Errorf("subscriberCountFast[%q] = %d, want 0 (mirror leaked)", key, fast)
	}
}

// TestCompleteSubscribe_CtxCancelMidFlightConsistent reproduces the specific
// interleaving #1806 describes: the hub ctx is cancelled concurrently with
// completeSubscribe, so the cancellation can land in the window between the
// pre-lock fast-fail and the under-lock re-check, driving the re-check branch.
// Across many iterations the subscription map and the per-key counter must stay
// mutually consistent — either a subscription is installed AND the counter is
// exactly 1 (successful subscribe), or no subscription is installed AND the
// counter is 0 (cleaned-up cancel). The pre-fix bug produced the forbidden
// fourth state: no subscription installed but counter still 1.
func TestCompleteSubscribe_CtxCancelMidFlightConsistent(t *testing.T) {
	key := "test:d:u:general"
	for iter := 0; iter < 300; iter++ {
		hub, router := newTestHub("")
		proc := session.NewTestProcess()
		sess := router.InjectSession(key, proc)

		c := &wsClient{
			send:          make(chan []byte, 8),
			done:          make(chan struct{}),
			subscriptions: make(map[string]func()),
			subGen:        make(map[string]uint64),
		}
		installPlaceholderSub(hub, c, key)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			hub.completeSubscribe(c, key, node.ClientMsg{Type: "subscribe", Key: key}, sess)
		}()
		go func() {
			defer wg.Done()
			hub.cancel()
		}()
		wg.Wait()

		cnt, hasSub, fast := ownerSubState(hub, c, key)
		switch {
		case hasSub && cnt != 1:
			t.Fatalf("iter %d: subscription installed but subscriberCount=%d, want 1", iter, cnt)
		case !hasSub && cnt != 0:
			// The exact #1806 over-count: cleanup branch dropped the
			// subscription but left the counter inflated.
			t.Fatalf("iter %d: no subscription installed but subscriberCount=%d, want 0 (counter leaked)", iter, cnt)
		}
		if fast != cnt {
			t.Fatalf("iter %d: subscriberCountFast=%d != subscriberCount=%d (mirror desync)", iter, fast, cnt)
		}
		hub.Shutdown()
	}
}
