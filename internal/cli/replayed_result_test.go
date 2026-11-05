package cli

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/shim"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// TestDeliverReplayedResult: the result of a turn that ended while naozhi
// was down sits in the drained backlog, which the read loop never
// dispatches. It reaches the result callback once, and only when it came
// after the watermark its turn began at; a live result reaches the callback
// bound through the hooks, even as the read loop's first frame.
func TestDeliverReplayedResult(t *testing.T) {
	const shimPID = 4242
	failed := `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"","session_id":"s1"}`
	// rvReplays(10, …) puts a result second in the backlog at seq 11.
	tests := []struct {
		name    string
		backlog []shim.ServerMsg
		live    []shim.ServerMsg
		after   TurnWatermark
		known   bool
		want    []string
	}{
		{"ended past the watermark", rvReplays(10, rvInit, rvResult), nil, TurnWatermark{shimPID, 10}, true, []string{"ok"}},
		{"from a shim started since", rvReplays(10, rvInit, rvResult), nil, TurnWatermark{1, 50}, true, []string{"ok"}},
		{"an earlier turn's result", rvReplays(10, rvInit, rvResult), nil, TurnWatermark{shimPID, 11}, true, nil},
		{"no watermark", rvReplays(10, rvInit, rvResult), nil, TurnWatermark{}, false, nil},
		{"failed turn", rvReplays(10, rvInit, failed), nil, TurnWatermark{shimPID, 10}, true, nil},
		{"still running: its result arrives live", rvReplays(10, rvInit, rvAgentToolUse),
			[]shim.ServerMsg{{Type: "stdout", Seq: 12, Line: rvResult}}, TurnWatermark{shimPID, 10}, true, []string{"ok"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newReconnectFixture(t, "s1", tc.backlog, tc.live...)
			f.handle.Hello.ShimPID = shimPID
			var mu sync.Mutex
			var got []string
			hooks := ReconnectHooks{OnResult: func(text string) { mu.Lock(); got = append(got, text); mu.Unlock() }}
			p, _, err := (&Wrapper{}).attachReconnected(context.Background(), f.handle, "planner:main", 0, &ClaudeProtocol{}, 0, 0, hooks)
			if err != nil {
				t.Fatalf("attachReconnected: %v", err)
			}
			f.drainWrites()
			defer p.Kill()
			if len(tc.live) > 0 {
				testhelper.Eventually(t, func() bool { return !p.AdoptedTurnPending() }, 3*time.Second, "live result never read")
			}
			for range 2 {
				p.DeliverReplayedResult(tc.after, tc.known)
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(got, tc.want) {
				t.Errorf("results = %q, want %q", got, tc.want)
			}
		})
	}
}
