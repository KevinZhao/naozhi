package node

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// reverseAuthWithCaps is reverseAuth plus the Capabilities the node advertises —
// the field the schema gate reads.
func reverseAuthWithCaps(t *testing.T, conn *websocket.Conn, nodeID, token string, caps []string) ReverseMsg {
	t.Helper()
	err := conn.WriteJSON(ReverseMsg{
		Type:         "register",
		NodeID:       nodeID,
		Token:        token,
		Hostname:     "worker.internal",
		Capabilities: caps,
	})
	if err != nil {
		t.Fatalf("write register: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var resp ReverseMsg
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("read register response: %v", err)
	}
	return resp
}

func startReverseServer(t *testing.T) (*ReverseServer, *httptest.Server, chan string) {
	t.Helper()
	rs := newTestReverseServer("node-1", "secret", false)
	registered := make(chan string, 1)
	rs.OnRegister = func(id string, conn *ReverseConn) { registered <- id }
	mux := http.NewServeMux()
	mux.Handle("/ws-node", rs)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return rs, srv, registered
}

// TestReverseServer_Register_rejectsForeignEventSchema is the gate the schema tag
// existed for and did not have. EventEntry is the payload of the event/events
// frames, so a node on another version does not lack a feature: every event it
// sends decodes into the wrong struct, silently. The link must not form.
func TestReverseServer_Register_rejectsForeignEventSchema(t *testing.T) {
	_, srv, registered := startReverseServer(t)

	conn := dialReverseNode(t, srv)
	defer conn.Close()

	resp := reverseAuthWithCaps(t, conn, "node-1", "secret", []string{"acp", "evententry.v99"})
	if resp.Type != "register_fail" {
		t.Fatalf("Type = %q, want register_fail — a node on a foreign event schema must not register", resp.Type)
	}
	// The operator has to be able to tell which side to upgrade, so both tags
	// belong in the message.
	if !strings.Contains(resp.Error, "evententry.v99") || !strings.Contains(resp.Error, clievent.SchemaCap) {
		t.Errorf("Error = %q, want both schema tags named", resp.Error)
	}

	select {
	case id := <-registered:
		t.Errorf("OnRegister fired for %q; a rejected node must never reach the registry", id)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestReverseServer_Register_acceptsCompatibleCaps covers the two shapes that are
// deliberately NOT a mismatch. Without them the gate would break the upgrade path
// it exists to protect: a node predating capability negotiation advertises no tag
// at all, and unknown feature caps are a WARN, not a schema disagreement.
func TestReverseServer_Register_acceptsCompatibleCaps(t *testing.T) {
	cases := []struct {
		name string
		caps []string
	}{
		{"our own tag", []string{clievent.SchemaCap}},
		{"no caps at all (pre-negotiation node)", nil},
		{"caps without any evententry tag", []string{"acp", "gemini"}},
		{"our tag plus a cap this binary never heard of", []string{clievent.SchemaCap, "some-future-backend"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, srv, registered := startReverseServer(t)
			conn := dialReverseNode(t, srv)
			defer conn.Close()

			resp := reverseAuthWithCaps(t, conn, "node-1", "secret", tc.caps)
			if resp.Type != "registered" {
				t.Fatalf("Type = %q (err %q), want registered", resp.Type, resp.Error)
			}
			select {
			case <-registered:
			case <-time.After(2 * time.Second):
				t.Error("OnRegister was not called")
			}
		})
	}
}

// TestReverseServer_Ack_carriesHubCaps: the node cannot discover what event schema
// the hub speaks any other way, and a hub predating the gate accepts anything — so
// the ack is the only place the node can learn it disagrees. Asserted separately
// from the gate because it is what makes the check symmetric.
func TestReverseServer_Ack_carriesHubCaps(t *testing.T) {
	_, srv, _ := startReverseServer(t)
	conn := dialReverseNode(t, srv)
	defer conn.Close()

	resp := reverseAuthWithCaps(t, conn, "node-1", "secret", []string{clievent.SchemaCap})
	if resp.Type != "registered" {
		t.Fatalf("Type = %q, want registered", resp.Type)
	}
	if clievent.SchemaCapMismatch(resp.Capabilities) != "" {
		t.Errorf("ack Capabilities = %v; a node applying the mirror gate would reject its own hub", resp.Capabilities)
	}
	var found bool
	for _, c := range resp.Capabilities {
		if c == clievent.SchemaCap {
			found = true
		}
	}
	if !found {
		t.Errorf("ack Capabilities = %v, want it to advertise %q", resp.Capabilities, clievent.SchemaCap)
	}
}
