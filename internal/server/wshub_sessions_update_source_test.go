package server

import (
	"encoding/json"
	"testing"

	"github.com/naozhi/naozhi/internal/node"
)

// TestSessionsUpdateMsg_DerivedFromServerMsg locks R243-ARCH-24 (#869): the
// pre-marshaled sessions_update broadcast body must be the SAME bytes
// json.Marshal produces from node.ServerMsg, so the frame can never drift
// from the wire schema when ServerMsg fields change.
func TestSessionsUpdateMsg_DerivedFromServerMsg(t *testing.T) {
	want, err := json.Marshal(node.ServerMsg{Type: "sessions_update"})
	if err != nil {
		t.Fatalf("marshal ServerMsg: %v", err)
	}
	if string(sessionsUpdateMsg) != string(want) {
		t.Fatalf("sessionsUpdateMsg = %q, want %q (drifted from node.ServerMsg)",
			sessionsUpdateMsg, want)
	}
	if string(sessionsUpdateMsg) != `{"type":"sessions_update"}` {
		t.Errorf("sessionsUpdateMsg = %q, want canonical {\"type\":\"sessions_update\"}", sessionsUpdateMsg)
	}
}
