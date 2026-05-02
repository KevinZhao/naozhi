package feishu

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
)

// TestAPIError_IsTokenExpired locks the catalogue of Feishu error codes that
// mean "tenant access token invalid / expired" so a future edit to the
// switch statement does not silently drop one and let ReplyWithRetry hammer
// the stale cached token. R83 / RETRY1.
func TestAPIError_IsTokenExpired(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code int
		want bool
	}{
		{99991671, true},  // tenant access token invalid
		{99991672, true},  // app access token invalid
		{99991673, true},  // user access token invalid
		{99991663, false}, // invalid app_secret (permanent, NOT token-expired)
		{99991664, false}, // app disabled (permanent)
		{99991668, false}, // not authorized (permanent)
		{1061045, false},  // bot not in chat (permanent)
		{230001, false},   // invalid receive_id (permanent)
		{11234, false},    // rate limit (transient, non-token)
		{0, false},        // success
	}
	for _, tc := range cases {
		e := &APIError{Code: tc.code, Op: "send"}
		if got := e.IsTokenExpired(); got != tc.want {
			t.Errorf("APIError{Code:%d}.IsTokenExpired() = %v, want %v", tc.code, got, tc.want)
		}
	}
}

// TestAPIError_TokenExpiredIsNotPermanent verifies the two classifications
// are disjoint: a token-expired error MUST NOT short-circuit retries via
// IsPermanent, otherwise the cache-invalidation path is unreachable.
func TestAPIError_TokenExpiredIsNotPermanent(t *testing.T) {
	t.Parallel()
	for _, code := range []int{99991671, 99991672, 99991673} {
		e := &APIError{Code: code, Op: "send"}
		if e.IsPermanent() {
			t.Errorf("code %d: IsPermanent() true — would short-circuit ReplyWithRetry before cache invalidation", code)
		}
		if !e.IsTokenExpired() {
			t.Errorf("code %d: IsTokenExpired() false — classification contract broken", code)
		}
	}
}

// TestFeishu_InvalidateAccessToken verifies the cache-clear helper removes
// both the token and the circuit-breaker state so the next getAccessToken
// call is not blocked by a stale tokenLastFailed. R83 / RETRY1.
func TestFeishu_InvalidateAccessToken(t *testing.T) {
	t.Parallel()
	f := &Feishu{
		accessToken:     "cached_token",
		tokenExpiry:     time.Now().Add(time.Hour),
		tokenLastFailed: fmt.Errorf("prior failure"),
		tokenLastFailAt: time.Now(),
	}
	f.invalidateAccessToken()
	if f.accessToken != "" {
		t.Errorf("accessToken = %q, want empty", f.accessToken)
	}
	if !f.tokenExpiry.IsZero() {
		t.Errorf("tokenExpiry = %v, want zero", f.tokenExpiry)
	}
	if f.tokenLastFailed != nil {
		t.Errorf("tokenLastFailed = %v, want nil (stale circuit-breaker must be cleared)", f.tokenLastFailed)
	}
	if !f.tokenLastFailAt.IsZero() {
		t.Errorf("tokenLastFailAt = %v, want zero", f.tokenLastFailAt)
	}
}

// TestFeishu_MaybeInvalidateOnTokenError_TokenExpired verifies the helper
// calls invalidateAccessToken when the error chain carries an APIError
// with a token-expired code.
func TestFeishu_MaybeInvalidateOnTokenError_TokenExpired(t *testing.T) {
	t.Parallel()
	f := &Feishu{
		accessToken: "cached_token",
		tokenExpiry: time.Now().Add(time.Hour),
	}
	// Wrap the APIError in fmt.Errorf("%w") to prove errors.As walks the chain.
	wrapped := fmt.Errorf("reply: %w", &APIError{Code: 99991671, Op: "send"})
	got := f.maybeInvalidateOnTokenError(wrapped)
	if got != wrapped {
		t.Errorf("returned err = %v, want %v (passthrough contract broken)", got, wrapped)
	}
	if f.accessToken != "" {
		t.Errorf("accessToken = %q, want empty (invalidation did not fire)", f.accessToken)
	}
}

// TestFeishu_MaybeInvalidateOnTokenError_PermanentError verifies the helper
// does NOT invalidate on permanent errors (e.g. invalid app_secret) — those
// are signalling config problems, not token staleness, so refreshing would
// just waste an HTTP round-trip.
func TestFeishu_MaybeInvalidateOnTokenError_PermanentError(t *testing.T) {
	t.Parallel()
	f := &Feishu{
		accessToken: "cached_token",
		tokenExpiry: time.Now().Add(time.Hour),
	}
	got := f.maybeInvalidateOnTokenError(&APIError{Code: 99991664, Op: "send"}) // app disabled
	if got == nil || !platform.IsPermanent(got) {
		t.Errorf("returned err = %v, want permanent APIError passthrough", got)
	}
	if f.accessToken != "cached_token" {
		t.Errorf("accessToken was cleared on non-token error — would churn refreshes for config problems")
	}
}

// TestFeishu_MaybeInvalidateOnTokenError_Nil verifies the no-op path — a
// nil error must not touch the token cache.
func TestFeishu_MaybeInvalidateOnTokenError_Nil(t *testing.T) {
	t.Parallel()
	f := &Feishu{
		accessToken: "cached_token",
		tokenExpiry: time.Now().Add(time.Hour),
	}
	if got := f.maybeInvalidateOnTokenError(nil); got != nil {
		t.Errorf("nil in, got %v out", got)
	}
	if f.accessToken != "cached_token" {
		t.Error("cached token was disturbed by nil error")
	}
}

// TestFeishu_MaybeInvalidateOnTokenError_NonAPIError verifies that generic
// errors (network timeouts, wrapped io errors) do not invalidate the cache.
// Only the structured APIError with a token code should trigger invalidation.
func TestFeishu_MaybeInvalidateOnTokenError_NonAPIError(t *testing.T) {
	t.Parallel()
	f := &Feishu{
		accessToken: "cached_token",
		tokenExpiry: time.Now().Add(time.Hour),
	}
	got := f.maybeInvalidateOnTokenError(fmt.Errorf("network: connection refused"))
	if got == nil {
		t.Error("non-nil error should pass through unchanged")
	}
	if f.accessToken != "cached_token" {
		t.Error("generic error invalidated token cache — over-eager invalidation")
	}
}

// Reference to silence unused imports if tests get restructured; also
// documents that context is a legitimate dependency for later Reply-level
// integration tests in this suite.
var _ = context.Background
