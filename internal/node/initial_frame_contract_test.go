package node

import (
	"os"
	"strings"
	"testing"
)

// TestRemoteHistoryFrames_MarkOpeningPage is the node-side half of the
// ServerMsg.Initial contract (the primary-side half lives in
// internal/server/static_subscription_recovery_contract_test.go).
//
// The dashboard treats Initial as authoritative when deciding whether a history
// frame replaces the whole events pane. That makes these two files the exact
// spots where a missing flag is invisible locally but blanks every
// reverse-connected / relayed session: their opening frames are the ONLY ones a
// remote session ever gets, and both are emitted from a goroutine that races
// the "subscribed" ack, so the client cannot fall back on arrival order.
//
// Conversely readLoop's `case "events"` relays a remote streamEvents batch —
// incremental by construction (the remote connector emits only "subscribed" on
// subscribe and pushes on EventLog append thereafter), so it must NOT set the
// flag or a live conversation gets full-page-replaced mid-turn.
func TestRemoteHistoryFrames_MarkOpeningPage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		file string
		// fn bounds the region to inspect; frames inside are opening frames.
		fn          string
		wantInitial bool
	}{
		{file: "reverseconn.go", fn: "func (c *ReverseConn) Subscribe(", wantInitial: true},
		{file: "relay.go", fn: "func (r *wsRelay) sendHistoryToClient(", wantInitial: true},
		{file: "reverseconn.go", fn: "func (c *ReverseConn) readLoop(", wantInitial: false},
	} {
		src, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		start := strings.Index(string(src), tc.fn)
		if start < 0 {
			t.Fatalf("%s: function %q not found", tc.file, tc.fn)
		}
		// Bound at the next top-level func so we only inspect this one.
		body := string(src)[start+len(tc.fn):]
		if end := strings.Index(body, "\nfunc "); end > 0 {
			body = body[:end]
		}
		found := false
		for _, line := range strings.Split(body, "\n") {
			if !strings.Contains(line, `Type: "history"`) {
				continue
			}
			found = true
			if has := strings.Contains(line, "Initial: true"); has != tc.wantInitial {
				t.Errorf("%s %s: history frame Initial=%v, want %v\n  %s",
					tc.file, tc.fn, has, tc.wantInitial, strings.TrimSpace(line))
			}
		}
		if !found && tc.wantInitial {
			t.Errorf("%s %s: expected at least one history frame to pin, found none — did the emitter move?", tc.file, tc.fn)
		}
	}
}
