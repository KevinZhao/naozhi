package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/node"
)

// TestHandleAuth_RateLimitEmitsRetryAfter pins the server-side contract that
// the front-end R110-P2 WS auth rate-limit countdown relies on. When the
// per-IP WSAuthLimiter denies an auth attempt, the hub must emit
// auth_fail with RetryAfter == wsAuthRetryAfterSeconds so the front-end
// can render the countdown instead of the legacy generic toast. Older
// clients ignore the extra field (omitempty keeps the payload compact
// for non-rate-limit paths), so this is a backwards-compatible addition.
func TestHandleAuth_RateLimitEmitsRetryAfter(t *testing.T) {
	t.Parallel()

	hub, _ := newTestHub("secret")
	// Always-deny limiter simulates a bucket that's already been drained —
	// this is the structural branch the UI relies on; the limiter policy
	// itself is orthogonal and covered by TestHandleLogin_Sets429AndRetryAfterOnRateLimit.
	hub.wsAuthLimiter = func(ip string) bool { return false }
	defer hub.Shutdown()

	c := &wsClient{
		send: make(chan []byte, 4),
		done: make(chan struct{}),
		hub:  hub,
	}
	hub.handleAuth(c, node.ClientMsg{Type: "auth", Token: "secret"})

	select {
	case raw := <-c.send:
		var msg node.ServerMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("unmarshal ServerMsg: %v (raw=%s)", err, raw)
		}
		if msg.Type != "auth_fail" {
			t.Errorf("type = %q, want auth_fail", msg.Type)
		}
		if msg.Error != "too many attempts" {
			t.Errorf("error = %q, want %q", msg.Error, "too many attempts")
		}
		if msg.RetryAfter != wsAuthRetryAfterSeconds {
			t.Errorf("retry_after = %d, want %d (front-end relies on this for countdown)", msg.RetryAfter, wsAuthRetryAfterSeconds)
		}
		// The raw JSON must carry the snake_case tag so dashboard.js's
		// msg.retry_after lookup resolves — structural assertion on the
		// wire format, not the Go field name.
		if !containsBytes(raw, []byte(`"retry_after":`)) {
			t.Errorf("raw payload missing retry_after field; got %s", raw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no auth_fail emitted within 2s")
	}
}

// TestHandleAuth_InvalidTokenOmitsRetryAfter pins the complementary contract:
// non-rate-limit auth failures (bad token) MUST NOT carry retry_after.
// Otherwise the front-end would misclassify a permanent auth failure as a
// transient rate-limit lockout and silently eat the user's re-login prompt
// behind a countdown.
func TestHandleAuth_InvalidTokenOmitsRetryAfter(t *testing.T) {
	t.Parallel()

	hub, _ := newTestHub("secret")
	// Limiter permissive so we land in the invalid-token branch, not the
	// rate-limit branch.
	hub.wsAuthLimiter = func(ip string) bool { return true }
	defer hub.Shutdown()

	c := &wsClient{
		send: make(chan []byte, 4),
		done: make(chan struct{}),
		hub:  hub,
	}
	hub.handleAuth(c, node.ClientMsg{Type: "auth", Token: "WRONG"})

	select {
	case raw := <-c.send:
		var msg node.ServerMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if msg.Type != "auth_fail" {
			t.Fatalf("type = %q, want auth_fail", msg.Type)
		}
		if msg.RetryAfter != 0 {
			t.Errorf("retry_after = %d on invalid-token branch, want 0 (omitempty)", msg.RetryAfter)
		}
		// Structural: omitempty means the key must be absent from the wire.
		if containsBytes(raw, []byte(`"retry_after":`)) {
			t.Errorf("raw payload must not carry retry_after on invalid-token branch; got %s", raw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no auth_fail emitted within 2s")
	}
}

func containsBytes(haystack, needle []byte) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
