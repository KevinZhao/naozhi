package dispatch

import (
	"errors"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestNewDispatcher_MissingSendReturnsError pins R250-ARCH-12: missing
// Send wireup must surface as an ErrSendWireupMissing return value, not
// a constructor-time panic. The previous implementation panicked, which
// took down the entire process before the systemd unit could log a clean
// boot diagnostic. Returning an error lets the caller (Server.Start) wrap
// it with context and exit cleanly.
//
// We assert three things to keep the contract tight:
//  1. NewDispatcher returns (nil, ErrSendWireupMissing) for empty config
//     (no Capabilities, no SendFn, AllowMissingSender=false).
//  2. errors.Is matches the sentinel — callers may rely on it for a
//     non-string check.
//  3. AllowMissingSender opts out and yields a non-nil dispatcher (the
//     existing test seam for headless builds keeps working).
func TestNewDispatcher_MissingSendReturnsError(t *testing.T) {
	t.Parallel()

	// Case 1: empty config — should fail with the sentinel.
	d, err := NewDispatcher(DispatcherConfig{})
	if err == nil {
		t.Fatal("NewDispatcher with empty config returned nil error; missing-Send wireup must surface as an error")
	}
	if !errors.Is(err, ErrSendWireupMissing) {
		t.Errorf("err = %v, want errors.Is(err, ErrSendWireupMissing)", err)
	}
	if d != nil {
		t.Errorf("dispatcher = %v, want nil on error", d)
	}

	// Case 2: AllowMissingSender opt-out — must succeed, returning a
	// usable dispatcher with NoopCapabilities installed. This locks the
	// existing test-seam contract used by headless tests.
	d2, err := NewDispatcher(DispatcherConfig{
		AllowMissingSender: true,
		Agents:             map[string]session.AgentOpts{"general": {}},
	})
	if err != nil {
		t.Fatalf("NewDispatcher with AllowMissingSender returned err=%v, want nil", err)
	}
	if d2 == nil {
		t.Fatal("AllowMissingSender returned nil dispatcher")
	}
}
