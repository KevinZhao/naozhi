package upstream

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// TestStreamEvents_NotifyClosedAfterReset_EmitsTerminalState pins RNEW-005:
// when the session has already been removed from the router (Reset()) by the
// time streamEvents observes notify being closed, we still MUST emit a final
// session_state message so the upstream primary knows the session ended and
// can trigger a re-subscribe on the next send. Prior code returned silently
// when GetSession(key) reported nil, leaving the primary believing the node
// still owned a live stream for the key.
func TestStreamEvents_NotifyClosedAfterReset_EmitsTerminalState(t *testing.T) {
	t.Parallel()

	r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	const key = "cron:stream-events-nil-test"
	r.RegisterCronStub(key, "/tmp/stream-events-test", "prompt")

	// Sanity: session is present before reset.
	if r.GetSession(key) == nil {
		t.Fatal("setup: RegisterCronStub did not install session")
	}

	c := &Connector{router: r}
	notify := make(chan struct{})

	// Simulate the lifecycle: Reset() removes the session from the router,
	// then the session's notify channel is closed. streamEvents should
	// observe GetSession(key)==nil but still emit a terminal session_state.
	r.Reset(key)
	if r.GetSession(key) != nil {
		t.Fatal("setup: Reset did not drop session")
	}

	// Capture what streamEvents writes. Nil is the bug (no terminal message).
	type captured struct {
		gotMsg *node.ReverseMsg
	}
	cap := &captured{}
	writeJSON := func(v any) error {
		if msg, ok := v.(node.ReverseMsg); ok {
			copy := msg
			cap.gotMsg = &copy
		}
		return nil
	}

	// Prime streamEvents by re-registering a stub so the initial GetSession
	// passes; the closed-notify branch is what we are testing.
	r.RegisterCronStub(key, "/tmp/stream-events-test", "prompt")
	if r.GetSession(key) == nil {
		t.Fatal("setup: re-register did not install session")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		// After entering the loop, drop the session again and close notify
		// to steer into the closed-channel branch with GetSession==nil.
		r.Reset(key)
		close(notify)
		// give streamEvents a beat to observe the closed notify
		time.Sleep(20 * time.Millisecond)
		close(done)
	}()

	c.streamEvents(ctx, writeJSON, key, notify)
	<-done

	if cap.gotMsg == nil {
		t.Fatal("RNEW-005: streamEvents returned without emitting a terminal session_state message when GetSession returned nil")
	}
	if cap.gotMsg.Type != "session_state" {
		t.Errorf("terminal msg type = %q, want %q", cap.gotMsg.Type, "session_state")
	}
	if cap.gotMsg.Key != key {
		t.Errorf("terminal msg key = %q, want %q", cap.gotMsg.Key, key)
	}
	// We require SOME non-empty state so the primary hub can route it
	// through the existing session_state handler (reverseconn.go:585-589).
	if cap.gotMsg.State == "" {
		t.Errorf("terminal msg state is empty; primary hub filters these — want a non-empty state (e.g. \"dead\")")
	}
}
