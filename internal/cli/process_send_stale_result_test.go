package cli

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// newStaleResultShim wires proto to a fake shim that records the frames the
// Process writes, like newPassthroughShim but for any protocol.
func newStaleResultShim(t *testing.T, proto Protocol) *passthroughShim {
	t.Helper()
	p, srv := shimTestPair(proto)
	s := &passthroughShim{srv: srv, proc: p, writeCh: make(chan shimClientMsg, 64)}
	s.readerWG.Add(1)
	go s.recvLoop()
	t.Cleanup(s.close)
	return s
}

// awaitStdinWrite blocks until the Process writes a user message to the shim.
func awaitStdinWrite(t *testing.T, s *passthroughShim) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case msg := <-s.writeCh:
			if msg.Type == "write" {
				return
			}
		case <-deadline:
			t.Error("no stdin write from Send")
			return
		}
	}
}

type sendOutcome struct {
	res *clievent.SendResult
	err error
}

func awaitSend(t *testing.T, out <-chan sendOutcome) *clievent.SendResult {
	t.Helper()
	select {
	case o := <-out:
		if o.err != nil {
			t.Fatalf("Send: %v", o.err)
		}
		return o.res
	case <-time.After(5 * time.Second):
		t.Fatal("Send did not return")
		return nil
	}
}

// A Send that claims the process in the gap between a mid-turn reconnect's
// result turning it Ready and that result reaching eventCh must answer with
// its own turn's result, not the reconnected one; the latch keeps the latter.
func TestSend_AfterMidTurnReconnectIgnoresAdoptedResult(t *testing.T) {
	codexResult := func(text string) []string {
		return []string{
			fmt.Sprintf(`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"t1","itemId":"i1","delta":%q}}`, text),
			`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t1","turn":{"status":"completed"}}}`,
		}
	}
	claudeResult := func(text string) []string {
		return []string{fmt.Sprintf(`{"type":"result","subtype":"success","session_id":"s1","result":%q}`, text)}
	}
	for _, tc := range []struct {
		name   string
		proto  Protocol
		result func(text string) []string
	}{
		{"claude", &ClaudeProtocol{}, claudeResult},
		{"codex", &CodexProtocol{BackendID: "codex"}, codexResult},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh := newStaleResultShim(t, tc.proto)
			armReconnectMidTurn(sh.proc)

			// onTurnDone runs on the read loop after Ready and before the
			// result is queued: start Send there and hold the read loop until
			// Send has drained and written, which is the window under test.
			out := make(chan sendOutcome, 1)
			var once sync.Once
			sh.proc.SetOnTurnDone(func() {
				once.Do(func() {
					go func() {
						res, err := sh.proc.Send(context.Background(), "next", nil, nil)
						out <- sendOutcome{res, err}
					}()
					awaitStdinWrite(t, sh)
				})
			})
			go sh.proc.readLoop()

			for _, line := range tc.result("OLD") {
				sh.srv.SendStdout(line)
			}
			for _, line := range tc.result("NEW") {
				sh.srv.SendStdout(line)
			}

			if res := awaitSend(t, out); res.Text != "NEW" {
				t.Errorf("Send returned %q, want NEW (its own turn's result)", res.Text)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			adopted, err := sh.proc.AdoptedOutcome(ctx)
			if err != nil {
				t.Fatalf("AdoptedOutcome: %v", err)
			}
			if adopted.End != AdoptedEndResult || adopted.Result.Text != "OLD" {
				t.Errorf("adopted outcome = %+v, want the reconnected turn's OLD result", adopted)
			}
		})
	}
}

// After an interrupt, drainStaleEvents holds back frames that arrive while it
// waits and queues them again; Send must still deliver those, since they
// arrived after it claimed the process.
func TestSend_InterruptHoldbackStillDelivered(t *testing.T) {
	sh := newStaleResultShim(t, &ClaudeProtocol{})
	sh.proc.turn.state = StateReady
	sh.proc.turn.interrupted.Store(true)
	sh.proc.turn.interruptedRun.Store(true)
	go sh.proc.readLoop()

	var mu sync.Mutex
	var toolEvents int
	onEvent := func(clievent.Event) {
		mu.Lock()
		toolEvents++
		mu.Unlock()
	}
	out := make(chan sendOutcome, 1)
	go func() {
		res, err := sh.proc.Send(context.Background(), "next", nil, onEvent)
		out <- sendOutcome{res, err}
	}()

	// drainStaleEvents clears the flags once it has taken its cutoff, so a
	// frame sent after that lands in its settle wait as a held-back event.
	testhelper.EventuallyWithInterval(t, func() bool {
		sh.proc.turn.mu.Lock()
		defer sh.proc.turn.mu.Unlock()
		return !sh.proc.turn.interrupted.Load()
	}, 3*time.Second, time.Millisecond, "Send never entered drainStaleEvents")
	sh.srv.SendStdout(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]}}`)
	sh.srv.SendStdout(`{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s1","result":"interrupted"}`)
	awaitStdinWrite(t, sh)
	sh.srv.SendStdout(`{"type":"result","subtype":"success","session_id":"s1","result":"NEW"}`)

	if res := awaitSend(t, out); res.Text != "NEW" {
		t.Errorf("Send returned %q, want NEW", res.Text)
	}
	mu.Lock()
	defer mu.Unlock()
	if toolEvents != 1 {
		t.Errorf("onEvent saw %d held-back tool_use events, want 1", toolEvents)
	}
}
