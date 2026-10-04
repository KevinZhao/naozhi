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
// Conversely readLoop's `case "events"` relays a remote streamEvents batch,
// incremental unless the node itself marked it as the page a want_history
// subscribe asked for, so it may only copy the node's flag, never set it, or a
// live conversation gets full-page-replaced mid-turn.
func TestRemoteHistoryFrames_MarkOpeningPage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		file string
		// fn bounds the region to inspect; frames inside are opening frames.
		fn          string
		wantInitial bool
	}{
		{file: "conn.go", fn: "func sendHistoryPage(", wantInitial: true},
		{file: "reverseconn.go", fn: "func (c *ReverseConn) readLoop(", wantInitial: false},
	} {
		body := funcSource(t, tc.file, tc.fn)
		found := false
		for _, line := range strings.Split(body, "\n") {
			if !strings.Contains(line, "wsproto.NewHistory(") {
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

	// Every opening frame a sink fetches for itself goes out through
	// sendHistoryPage, so the pin above covers them all.
	for _, tc := range []struct{ file, fn string }{
		{"reverseconn.go", "func (c *ReverseConn) Subscribe("},
		{"relay.go", "func (r *wsRelay) sendHistoryToClient("},
	} {
		body := funcSource(t, tc.file, tc.fn)
		if !strings.Contains(body, "sendHistoryPage(") || strings.Contains(body, "wsproto.NewHistory(") {
			t.Errorf("%s %s: opening frames must go out through sendHistoryPage", tc.file, tc.fn)
		}
	}
}

// funcSource returns fn's source in file, bounded at the next top-level func.
func funcSource(t *testing.T, file, fn string) string {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	start := strings.Index(string(src), fn)
	if start < 0 {
		t.Fatalf("%s: function %q not found", file, fn)
	}
	body := string(src)[start+len(fn):]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	return body
}
