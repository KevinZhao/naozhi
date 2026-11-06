package cli

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/shim"
)

// TestAttachReconnected_OnResultBeforeReadLoop: the turn in flight at
// reconnect may answer with the read loop's first frame, before
// SpawnReconnect returns to a caller that could bind SetOnResult.
func TestAttachReconnected_OnResultBeforeReadLoop(t *testing.T) {
	f := newReconnectFixture(t, "s1", rvReplays(1, rvInit, rvAgentToolUse),
		shim.ServerMsg{Type: "stdout", Seq: 3, Line: rvResult})
	got := make(chan string, 1)
	hooks := ReconnectHooks{OnResult: func(res clievent.SendResult) { got <- res.Text }}
	p, _, err := (&Wrapper{}).attachReconnected(context.Background(), f.handle, "k", 0, &ClaudeProtocol{}, 0, 0, hooks)
	if err != nil {
		t.Fatalf("attachReconnected: %v", err)
	}
	defer p.Kill()
	f.drainWrites()
	select {
	case text := <-got:
		if text != "ok" {
			t.Errorf("OnResult got %q, want the live result", text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OnResult never got the live result")
	}
}

// TestReplayedResult: only a result the backlog ended in is one no read
// loop delivers; a mid-turn reconnect's result comes live.
func TestReplayedResult(t *testing.T) {
	failed := `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s1"}`
	for _, tc := range []struct {
		name    string
		backlog []shim.ServerMsg
		text    string
		ok      bool
	}{
		{"ended while down", rvReplays(1, rvInit, rvAgentToolUse, rvResult, rvProgress), "ok", true},
		{"mid turn", rvReplays(1, rvResult, rvInit, rvAgentToolUse), "", false},
		{"idle", nil, "", false},
		{"failed", rvReplays(1, rvInit, failed), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReconnectFixture(t, "s1", tc.backlog)
			p, _, err := (&Wrapper{}).attachReconnected(context.Background(), f.handle, "k", 0, &ClaudeProtocol{}, 0, 0, ReconnectHooks{})
			if err != nil {
				t.Fatalf("attachReconnected: %v", err)
			}
			f.drainWrites()
			defer p.Kill()
			if text, ok := p.ReplayedResult(); text != tc.text || ok != tc.ok {
				t.Errorf("ReplayedResult = (%q, %v), want (%q, %v)", text, ok, tc.text, tc.ok)
			}
		})
	}
}
