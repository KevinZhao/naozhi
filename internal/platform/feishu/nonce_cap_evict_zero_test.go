package feishu

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

// newCapHitFeishu builds a token-mode Feishu wired to a mux, with the nonce
// counter pinned at the cap and eviction stubbed to return `evicted`. Driving
// a single webhook through the returned mux exercises the cap-hit recovery
// path in serveWebhook deterministically (no 50k-entry seeding needed).
func newCapHitFeishu(t *testing.T, evicted int) (*Feishu, *http.ServeMux) {
	t.Helper()
	f := New(Config{
		AppID: "id", AppSecret: "secret",
		ConnectionMode:       "webhook",
		VerificationToken:    "test_token",
		AllowInsecureWebhook: true,
	}, nil)
	f.evictNoncesFn = func() int { return evicted }
	// Pin the counter at the cap so the line-226 speculative Add(1) trips
	// the n > maxSeenNonces branch on every delivery.
	f.seenNoncesCount.Store(maxSeenNonces)
	mux := http.NewServeMux()
	f.registerWebhook(mux, func(ctx context.Context, msg platform.IncomingMessage) {})
	return f, mux
}

// TestWebhook_CapHitEvictZero_NoCounterDrift pins issue #1767: when the nonce
// map is at cap and evictOldestNonces returns 0 (e.g. the cleanup ticker just
// swept the map between the cap-check and our eviction), the request must 429
// AND the speculative +1 reservation must be fully rolled back. The pre-fix
// code did a second Add(1) before the evicted==0 check and only one Add(-1),
// leaking +1 per such request and ratcheting the counter past the cap forever.
func TestWebhook_CapHitEvictZero_NoCounterDrift(t *testing.T) {
	f, mux := newCapHitFeishu(t, 0)

	const requests = 50
	for i := 0; i < requests; i++ {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, buildTokenRequest(buildV2MessageBody("ev_capzero", "oc_chat1", "p2p", "hi")))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("request %d: status = %d; want 429", i, w.Code)
		}
	}

	// The counter must be exactly back at the cap — not cap+requests.
	if got := f.seenNoncesCount.Load(); got != int64(maxSeenNonces) {
		t.Fatalf("seenNoncesCount drifted to %d after %d evicted==0 requests; want %d",
			got, requests, maxSeenNonces)
	}
}

// TestWebhook_CapHitEvictZeroConcurrent_NoCounterDrift runs the evicted==0 path
// concurrently to catch any drift under racing reserve/rollback on the counter.
func TestWebhook_CapHitEvictZeroConcurrent_NoCounterDrift(t *testing.T) {
	f, mux := newCapHitFeishu(t, 0)

	const goroutines = 32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, buildTokenRequest(buildV2MessageBody("ev_capzero_c", "oc_chat1", "p2p", "hi")))
			if w.Code != http.StatusTooManyRequests {
				t.Errorf("status = %d; want 429", w.Code)
			}
		}()
	}
	wg.Wait()

	if got := f.seenNoncesCount.Load(); got != int64(maxSeenNonces) {
		t.Fatalf("seenNoncesCount drifted to %d under concurrent evicted==0; want %d",
			got, maxSeenNonces)
	}
}

// TestWebhook_CapHitEvictPositive_ReservesOneSlot verifies the evicted>0 branch
// still works: eviction frees headroom, the request inserts its nonce, and the
// counter nets +1 relative to the post-evict size (one new live entry).
func TestWebhook_CapHitEvictPositive_ReservesOneSlot(t *testing.T) {
	f, mux := newCapHitFeishu(t, 1)
	// Simulate eviction having reclaimed headroom: drop the counter below cap
	// so the post-evict Add(1) lands under maxSeenNonces and the insert sticks.
	f.seenNoncesCount.Store(maxSeenNonces - 10)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, buildTokenRequest(buildV2MessageBody("ev_cappos", "oc_chat1", "p2p", "hi")))

	// A fresh nonce under the (re-opened) cap must be accepted, not 429'd.
	if w.Code == http.StatusTooManyRequests {
		t.Fatalf("evicted>0 with headroom should not 429; got %d", w.Code)
	}
	// Exactly one new nonce should have landed in the map.
	if got := liveNonceCount(f); got != 1 {
		t.Fatalf("expected exactly 1 live nonce after accepted insert, got %d", got)
	}
	// Counter must reflect the single live entry, not a leaked reservation.
	if got := f.seenNoncesCount.Load(); got != int64(maxSeenNonces-10+1) {
		t.Fatalf("seenNoncesCount = %d; want %d", got, maxSeenNonces-10+1)
	}
}
