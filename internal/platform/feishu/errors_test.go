package feishu

import "testing"

// TestAPIError_Classification pins which Feishu codes short-circuit retries
// (permanent) and which ask for a token refresh (token-expired). The two must
// stay disjoint: a token-expired code classified permanent would stop
// ReplyWithRetry before the cache invalidation could help.
func TestAPIError_Classification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name               string
		code               int
		permanent, expired bool
	}{
		{"invalid_app_secret", 99991663, true, false},
		{"app_disabled", 99991664, true, false},
		{"app_not_authorized", 99991668, true, false},
		{"bot_not_in_chat", 1061045, true, false},
		{"invalid_receive_id", 230001, true, false},
		{"tenant_token_invalid", 99991671, false, true},
		{"app_token_invalid", 99991672, false, true},
		{"user_token_invalid", 99991673, false, true},
		{"rate_limit_transient", 11234, false, false},
		{"server_side_retriable", 99991400, false, false},
		{"success", 0, false, false},
	}
	for _, tc := range tests {
		e := &APIError{Code: tc.code, Msg: "x", Op: "send"}
		if got := e.IsPermanent(); got != tc.permanent {
			t.Errorf("%s: IsPermanent() = %v, want %v", tc.name, got, tc.permanent)
		}
		if got := e.IsTokenExpired(); got != tc.expired {
			t.Errorf("%s: IsTokenExpired() = %v, want %v", tc.name, got, tc.expired)
		}
	}
}
