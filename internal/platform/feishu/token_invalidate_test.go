package feishu

import (
	"fmt"
	"testing"
	"time"
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

// TestFeishu_MaybeInvalidateOnTokenError: the helper passes the error through
// unchanged and clears the token cache only when the error chain carries an
// APIError with a token-expired code. Permanent errors (config problems) and
// generic errors (network) must not churn refreshes. R83 / RETRY1.
func TestFeishu_MaybeInvalidateOnTokenError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		err         error
		wantCleared bool
	}{
		// Wrapped in fmt.Errorf("%w") to prove errors.As walks the chain.
		{"token expired", fmt.Errorf("reply: %w", &APIError{Code: 99991671, Op: "send"}), true},
		{"permanent (app disabled)", &APIError{Code: 99991664, Op: "send"}, false},
		{"nil", nil, false},
		{"non-API error", fmt.Errorf("network: connection refused"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &Feishu{
				accessToken: "cached_token",
				tokenExpiry: time.Now().Add(time.Hour),
			}
			if got := f.maybeInvalidateOnTokenError(tt.err); got != tt.err {
				t.Errorf("returned err = %v, want %v (passthrough contract broken)", got, tt.err)
			}
			if cleared := f.accessToken == ""; cleared != tt.wantCleared {
				t.Errorf("token cleared = %v, want %v", cleared, tt.wantCleared)
			}
		})
	}
}
