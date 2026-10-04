package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clierr"
)

// Frames captured from claude 2.1.288 (`-p --output-format stream-json
// --verbose`, Bash `sleep 75`), trimmed to the fields naozhi reads. The
// heartbeat's own tool_use_id is per-beat; parent_tool_use_id is the tool.
const (
	frameBashToolUse   = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_bdrk_01HcdQJfAMtsSjqrwmyGFh32","name":"Bash","input":{"command":"sleep 75 && echo done"}}]},"parent_tool_use_id":null,"session_id":"s1"}`
	frameBashHeartbeat = `{"type":"tool_progress","tool_use_id":"toolu_bdrk_01HcdQJfAMtsSjqrwmyGFh32-heartbeat-0","tool_name":"Bash","parent_tool_use_id":"toolu_bdrk_01HcdQJfAMtsSjqrwmyGFh32","elapsed_time_seconds":30,"heartbeat":true,"session_id":"s1","uuid":"1b061073-26d8-4c18-9186-300a31acbc05"}`
	frameBashResult    = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_bdrk_01HcdQJfAMtsSjqrwmyGFh32","type":"tool_result","content":"done","is_error":false}]},"parent_tool_use_id":null,"session_id":"s1"}`
)

// watchdogTestNoOutput is far below the watchdog's 1s check-interval floor, so
// the first tick (about 1s into the turn) kills unless output arrived since.
const watchdogTestNoOutput = 300 * time.Millisecond

// startWatchdogTurn starts a Send with a short no-output budget and returns
// once the user message reached the fake shim, so frames sent afterwards
// belong to the turn.
func startWatchdogTurn(t *testing.T) (*shimTestServer, <-chan error) {
	t.Helper()
	p, srv := shimTestPair(&ClaudeProtocol{})
	p.noOutputTimeout = watchdogTestNoOutput
	writes := startServerWriteTap(srv)
	p.startReadLoop()
	t.Cleanup(func() { p.Kill(); srv.Close() })
	errCh := make(chan error, 1)
	go func() {
		_, err := p.Send(context.Background(), "hello", nil, nil)
		errCh <- err
	}()
	awaitWrite(t, writes)
	return srv, errCh
}

func awaitSendErr(t *testing.T, errCh <-chan error) error {
	t.Helper()
	select {
	case err := <-errCh:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Send did not return")
		return nil
	}
}

// A tool that runs past the no-output budget survives on tool_progress
// heartbeats alone: they are the liveness signal for long tools.
func TestSend_ToolProgressHeartbeatKeepsLongToolAlive(t *testing.T) {
	srv, errCh := startWatchdogTurn(t)
	srv.SendStdout(frameBashToolUse)
	beat := time.NewTicker(watchdogTestNoOutput / 3)
	defer beat.Stop()
	until := time.After(1600 * time.Millisecond) // spans the first watchdog tick
	for running := true; running; {
		select {
		case <-beat.C:
			srv.SendStdout(frameBashHeartbeat)
		case <-until:
			running = false
		case err := <-errCh:
			t.Fatalf("Send returned mid-tool with heartbeats flowing: %v", err)
		}
	}
	srv.SendStdout(frameBashResult)
	srv.SendStdout(`{"type":"result","result":"done","session_id":"s1"}`)
	if err := awaitSendErr(t, errCh); err != nil {
		t.Fatalf("Send = %v, want the turn's result", err)
	}
}

// A tool that goes silent is named in the timeout error.
func TestSend_NoOutputTimeoutNamesSilentTool(t *testing.T) {
	srv, errCh := startWatchdogTurn(t)
	srv.SendStdout(frameBashToolUse)
	srv.SendStdout(frameBashHeartbeat)

	err := awaitSendErr(t, errCh)
	if !errors.Is(err, clierr.ErrNoOutputTimeout) {
		t.Fatalf("Send = %v, want ErrNoOutputTimeout", err)
	}
	var nt *clierr.NoOutputTimeoutError
	if !errors.As(err, &nt) {
		t.Fatalf("Send = %T %v, want a *clierr.NoOutputTimeoutError", err, err)
	}
	if nt.Tool != "Bash" || nt.Timeout != watchdogTestNoOutput {
		t.Errorf("timeout error = %+v, want Tool Bash and Timeout %v", nt, watchdogTestNoOutput)
	}
	if nt.ToolElapsed < watchdogTestNoOutput {
		t.Errorf("ToolElapsed = %v, want at least the %v budget the tool was silent for", nt.ToolElapsed, watchdogTestNoOutput)
	}
}

// Once the tool's result is in, later silence is the model's; the heartbeat
// seen during the tool must not leave a phantom tool behind.
func TestSend_NoOutputTimeoutAfterToolResultBlamesModel(t *testing.T) {
	srv, errCh := startWatchdogTurn(t)
	srv.SendStdout(frameBashToolUse)
	srv.SendStdout(frameBashHeartbeat)
	srv.SendStdout(frameBashResult)

	err := awaitSendErr(t, errCh)
	var nt *clierr.NoOutputTimeoutError
	if !errors.As(err, &nt) {
		t.Fatalf("Send = %T %v, want a *clierr.NoOutputTimeoutError", err, err)
	}
	if nt.Tool != "" {
		t.Errorf("Tool = %q after the tool's result arrived, want empty (the model went silent)", nt.Tool)
	}
}

// A passthrough turn shares the verdict, so its callers also learn which tool
// went silent.
func TestPassthroughWatchdog_NoOutputTimeoutNamesSilentTool(t *testing.T) {
	sh := startWatchdogShim(t, 80*time.Millisecond, time.Minute, fastWatchdog)
	uuid, out := sh.sendAsync(t, context.Background(), "A")
	sh.emitInit("s1")
	sh.emitReplay(uuid, "A")
	sh.srv.SendStdout(frameBashToolUse)

	o := waitOut(t, "A", out, 3*time.Second)
	var nt *clierr.NoOutputTimeoutError
	if !errors.As(o.err, &nt) || nt.Tool != "Bash" {
		t.Fatalf("SendPassthrough err = %v, want a *clierr.NoOutputTimeoutError naming Bash", o.err)
	}
}

// The heartbeat must survive ReadEventInto with the fields the tracker keys on,
// and must stay out of the dashboard transcript.
func TestClaudeProtocol_ToolProgressFrameContract(t *testing.T) {
	evs, isResult, err := (&ClaudeProtocol{}).ReadEvent(frameBashHeartbeat)
	if err != nil || isResult || len(evs) != 1 {
		t.Fatalf("ReadEvent(tool_progress) = %v, %v, %v; want one non-result event", evs, isResult, err)
	}
	ev := evs[0]
	if ev.Type != "tool_progress" || ev.ToolName != "Bash" || ev.ParentToolUseID != "toolu_bdrk_01HcdQJfAMtsSjqrwmyGFh32" {
		t.Errorf("tool_progress parsed as Type=%q ToolName=%q ParentToolUseID=%q", ev.Type, ev.ToolName, ev.ParentToolUseID)
	}
	if entries := EventEntriesFromEventAt(ev, 1); len(entries) != 0 {
		t.Errorf("tool_progress produced %d transcript entries, want none: %+v", len(entries), entries)
	}

	evs, _, err = (&ClaudeProtocol{}).ReadEvent(frameBashResult)
	if err != nil || len(evs) != 1 || evs[0].Message == nil || len(evs[0].Message.Content) != 1 {
		t.Fatalf("ReadEvent(tool_result) = %v, %v", evs, err)
	}
	if got := evs[0].Message.Content[0]; got.Type != "tool_result" || got.ToolUseID != "toolu_bdrk_01HcdQJfAMtsSjqrwmyGFh32" {
		t.Errorf("tool_result block = %+v, want its tool_use_id", got)
	}
}
