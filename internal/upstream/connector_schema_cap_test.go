package upstream

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
)

// ackWith replies to the register frame with a "registered" ack carrying caps.
func ackWith(t *testing.T, caps []string) *httptest.Server {
	t.Helper()
	return newFakeServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			return
		}
		var reg node.ReverseMsg
		if err := conn.ReadJSON(&reg); err != nil {
			return
		}
		_ = conn.WriteJSON(node.ReverseMsg{Type: "registered", Capabilities: caps})
		// The deferred Close lands right after the ack, which is enough: runOnce
		// reports connected=true as soon as it accepts the ack (handleConn's own
		// error is returned alongside), so the assertion does not need the socket
		// held open — and holding it open would only be a fixed sleep.
	})
}

// TestRunOnce_RejectsForeignHubEventSchema is the half of the gate the hub cannot
// do for itself: a hub predating this check accepts any node, so the node has to
// refuse. Without it the node would sit in a reconnect-free "connected" state
// decoding every event frame into the wrong shape.
func TestRunOnce_RejectsForeignHubEventSchema(t *testing.T) {
	srv := ackWith(t, []string{"evententry.v99"})

	cfg := &Config{URL: wsURL(srv), NodeID: "node1", Token: "tok"}
	c := New(cfg, makeRouter(), nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	connected, err := c.runOnce(ctx)
	if connected {
		t.Error("connected = true; a hub on a foreign event schema must not count as a successful session")
	}
	if err == nil {
		t.Fatal("err = nil, want a schema mismatch error")
	}
	// Both tags, so the operator knows which side is older.
	if !strings.Contains(err.Error(), "evententry.v99") || !strings.Contains(err.Error(), clievent.SchemaCap) {
		t.Errorf("err = %v, want both schema tags named", err)
	}
}

// TestRunOnce_AcceptsCompatibleHubCaps: an ack with no caps is a hub predating the
// symmetric handshake. It speaks v1 by construction, and refusing it would strand
// every node against an older primary — the opposite of what the tag is for.
func TestRunOnce_AcceptsCompatibleHubCaps(t *testing.T) {
	cases := []struct {
		name string
		caps []string
	}{
		{"hub advertises our tag", []string{clievent.SchemaCap}},
		{"hub predating the symmetric ack sends no caps", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := ackWith(t, tc.caps)
			cfg := &Config{URL: wsURL(srv), NodeID: "node1", Token: "tok"}
			c := New(cfg, makeRouter(), nil, nil)

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			connected, err := c.runOnce(ctx)
			if !connected {
				t.Errorf("connected = false (err %v); a compatible hub must register", err)
			}
		})
	}
}
