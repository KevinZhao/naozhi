package feishu

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
)

// TestReply_ImageTokenExpiry_PropagatesError is the R20260623-LB-3 (#2305)
// regression guard. A single-image Feishu reply that hits token-expiry
// (99991671) must propagate a TokenInvalidatedError out of Reply so the
// caller's platform.ReplyWithRetry can grant its one-shot rotation retry.
//
// Before the fix the image branch swallowed the error to nil (log-and-
// continue), so the only image was lost silently: the cache was cleared but
// nothing retried with the rotated token — an asymmetry with the text path.
func TestReply_ImageTokenExpiry_PropagatesError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The image upload endpoint returns the token-expired code.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":99991671,"msg":"tenant access token invalid"}`))
	}))
	defer srv.Close()

	f := &Feishu{
		baseURL:     srv.URL,
		accessToken: "cached_token",
		tokenExpiry: time.Now().Add(time.Hour), // valid cache → no token fetch
	}

	_, err := f.Reply(context.Background(), platform.OutgoingMessage{
		ChatID: "chat1",
		Images: []platform.Image{{Data: []byte("png-bytes"), MimeType: "image/png"}},
	})
	if err == nil {
		t.Fatal("#2305: image token-expiry swallowed to nil — single image lost silently")
	}
	if !platform.IsTokenInvalidated(err) {
		t.Fatalf("#2305: returned err = %v, want a TokenInvalidatedError so ReplyWithRetry retries", err)
	}
	// Side effect must still fire so the retry carries a fresh token.
	if f.accessToken != "" {
		t.Errorf("token cache not invalidated on image token-expiry; got %q", f.accessToken)
	}
}

// TestReply_ImageNonTokenError_LogAndContinue pins the negative case: a
// non-token image failure keeps the log-and-continue behaviour and Reply
// returns nil (the text part, if any, already landed and the next image can
// still be attempted). Only token errors short-circuit to the retry path.
func TestReply_ImageNonTokenError_LogAndContinue(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 11234 = rate limit: transient, non-token, non-permanent.
		_, _ = w.Write([]byte(`{"code":11234,"msg":"rate limited"}`))
	}))
	defer srv.Close()

	f := &Feishu{
		baseURL:     srv.URL,
		accessToken: "cached_token",
		tokenExpiry: time.Now().Add(time.Hour),
	}

	_, err := f.Reply(context.Background(), platform.OutgoingMessage{
		ChatID: "chat1",
		Images: []platform.Image{{Data: []byte("png-bytes"), MimeType: "image/png"}},
	})
	if err != nil {
		t.Fatalf("non-token image failure must stay log-and-continue (nil); got %v", err)
	}
	if f.accessToken != "cached_token" {
		t.Error("non-token error invalidated the token cache — over-eager invalidation")
	}
}
