package feishu

import (
	"errors"
	"fmt"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

func TestAPIError_IsPermanent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		code int
		want bool
	}{
		{"invalid_app_secret", 99991663, true},
		{"app_disabled", 99991664, true},
		{"app_not_authorized", 99991668, true},
		{"bot_not_in_chat", 1061045, true},
		{"invalid_receive_id", 230001, true},
		{"rate_limit_transient", 11234, false},
		{"server_side_retriable", 99991400, false},
		{"success", 0, false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &APIError{Code: tc.code, Msg: "x", Op: "send"}
			if got := e.IsPermanent(); got != tc.want {
				t.Fatalf("APIError{Code:%d}.IsPermanent() = %v, want %v", tc.code, got, tc.want)
			}
		})
	}
}

// TestAPIError_IntegratesWithPlatformIsPermanent guards the contract between
// the platform.PermanentError interface and the feishu error type. A change
// to either signature would break retry-loop short-circuiting across the
// whole project.
func TestAPIError_IntegratesWithPlatformIsPermanent(t *testing.T) {
	t.Parallel()
	permanent := &APIError{Code: 99991664, Op: "send"}
	wrapped := fmt.Errorf("reply failed: %w", permanent)
	if !platform.IsPermanent(wrapped) {
		t.Fatalf("platform.IsPermanent should see through wrapped APIError")
	}
	// Also validate errors.As — HTTP handlers and tests will walk the chain
	// explicitly to read the Code.
	var api *APIError
	if !errors.As(wrapped, &api) {
		t.Fatalf("errors.As did not extract *APIError from wrapped chain")
	}
	if api.Code != 99991664 {
		t.Fatalf("extracted code = %d, want 99991664", api.Code)
	}
}
