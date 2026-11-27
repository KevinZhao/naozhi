package feishu

import (
	"fmt"
	"testing"
	"time"
)

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
