package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// TestHandleSend_RateLimit429BodiesAreDistinct pins F6: the dashboard used to
// label every 429 "消息队列已满" although the only HTTP 429 sources are the
// per-IP send / upload limiters. Each limiter must return its own
// user-facing label so the client can just display the body.
func TestHandleSend_RateLimit429BodiesAreDistinct(t *testing.T) {
	hub, _ := newTestHub("")
	t.Cleanup(hub.Shutdown)
	h := &SendHandler{
		hub:         hub,
		uploadStore: newUploadStore(),
		sendLimiter: newIPLimiterWithProxy(rate.Every(time.Hour), 1, false),
	}
	first := postSendJSON(t, h, "tok", map[string]any{"key": "feishu:p2p:u1", "text": "hi"})
	if first.Code != http.StatusAccepted {
		t.Fatalf("first request must pass the burst-1 limiter, got %d", first.Code)
	}
	second := postSendJSON(t, h, "tok", map[string]any{"key": "feishu:p2p:u1", "text": "hi"})
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", second.Code)
	}
	if !strings.Contains(second.Body.String(), sendRateLimitedMsg) || sendRateLimitedMsg == uploadRateLimitedMsg {
		t.Fatalf("429 body must carry the send-limiter label: %s", second.Body.String())
	}
	if strings.Contains(sendRateLimitedMsg, "队列") {
		t.Fatalf("send-limiter label must not claim the queue is full: %q", sendRateLimitedMsg)
	}
}
