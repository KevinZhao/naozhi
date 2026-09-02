package cli

// protocol_setmodel_test.go — wire-shape and response-interception contracts
// for the runtime model-switch channel (ModelSetter facet).
// docs/rfc/dashboard-model-effort-control.md §4.4 / §5.
//
// The load-bearing test here is the ACP interception one: without the
// pendingControl table, a session/set_model response frame would hit
// ReadEvent's generic IsResponse branch and CLOSE THE ACTIVE TURN — the
// mid-turn safety verified live as F13 depends on naozhi-side routing, not
// just on kiro's behaviour.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestACPProtocol_WriteSetModel_Wire pins the session/set_model request
// shape (F1): {"jsonrpc":"2.0","id":N,"method":"session/set_model",
// "params":{"sessionId":…,"modelId":…}}.
func TestACPProtocol_WriteSetModel_Wire(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	p.storeSessionID("sess-1")
	var buf bytes.Buffer
	if err := p.WriteSetModel(&buf, "req-7", "claude-haiku-4.5"); err != nil {
		t.Fatal(err)
	}
	line := buf.String()
	if !strings.HasSuffix(line, "\n") {
		t.Error("frame must be newline-terminated (NDJSON framing)")
	}
	var req struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  struct {
			SessionID string `json:"sessionId"`
			ModelID   string `json:"modelId"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		t.Fatalf("frame not valid JSON: %v\n%s", err, line)
	}
	if req.Method != "session/set_model" {
		t.Errorf("method = %q, want session/set_model", req.Method)
	}
	if req.Params.SessionID != "sess-1" || req.Params.ModelID != "claude-haiku-4.5" {
		t.Errorf("params = %+v", req.Params)
	}
	// The RPC id must be registered for response interception.
	if reqID, ok := p.takePendingControl(req.ID); !ok || reqID != "req-7" {
		t.Errorf("pendingControl[%d] = (%q,%v), want (req-7,true)", req.ID, reqID, ok)
	}
}

// TestACPProtocol_WriteSetModel_NoSession mirrors WriteInterrupt's
// pre-handshake contract: no session id yet → ErrSetModelUnsupported so the
// caller records the override for the next spawn instead.
func TestACPProtocol_WriteSetModel_NoSession(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	var buf bytes.Buffer
	if err := p.WriteSetModel(&buf, "req-1", "m"); err != ErrSetModelUnsupported {
		t.Fatalf("err = %v, want ErrSetModelUnsupported", err)
	}
	if buf.Len() != 0 {
		t.Error("nothing must be written pre-handshake")
	}
}

// TestACPProtocol_ReadEvent_SetModelResponseIsAckNotTurnEnd is the mid-turn
// truncation regression guard (F13). A response frame whose id matches an
// in-flight set_model MUST surface as control_ack — never as the
// (assistant text + result) turn-end pair the generic IsResponse branch
// produces.
func TestACPProtocol_ReadEvent_SetModelResponseIsAckNotTurnEnd(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	p.storeSessionID("sess-1")

	// Simulate accumulated mid-turn text so a false turn-end would be
	// observable as a flushed assistant event.
	if _, _, err := p.ReadEvent(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"partial"}}}}`); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := p.WriteSetModel(&buf, "req-9", "claude-sonnet-4.6"); err != nil {
		t.Fatal(err)
	}
	var req struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(buf.Bytes(), &req); err != nil {
		t.Fatal(err)
	}

	// Success response for the set_model id.
	events, done, err := p.ReadEvent(`{"jsonrpc":"2.0","result":{},"id":` + itoa(req.ID) + `}`)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Error("set_model response must not set done")
	}
	if len(events) != 1 || events[0].Type != "control_ack" {
		t.Fatalf("events = %+v, want single control_ack", events)
	}
	if events[0].SubType != "success" || events[0].RPCRequestID != "req-9" {
		t.Errorf("ack = %+v, want success/req-9", events[0])
	}

	// The turn text must still be buffered: a later REAL prompt response
	// flushes it — proving the interception did not consume the turn.
	events, _, err = p.ReadEvent(`{"jsonrpc":"2.0","result":{"stopReason":"end_turn"},"id":999}`)
	if err != nil {
		t.Fatal(err)
	}
	var sawText bool
	for _, ev := range events {
		if ev.Message != nil {
			for _, c := range ev.Message.Content {
				if strings.Contains(c.Text, "partial") {
					sawText = true
				}
			}
		}
	}
	if !sawText {
		t.Error("turn text was lost — set_model interception must not flush textBuf")
	}
}

// TestACPProtocol_ReadEvent_SetModelErrorResponseIsErrorAck covers the error
// shape: the ack carries subtype error + sanitized message, and does NOT go
// down the ErrACPRPC turn-failure path.
func TestACPProtocol_ReadEvent_SetModelErrorResponseIsErrorAck(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	p.storeSessionID("sess-1")
	var buf bytes.Buffer
	if err := p.WriteSetModel(&buf, "req-3", "bad"); err != nil {
		t.Fatal(err)
	}
	var req struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(buf.Bytes(), &req); err != nil {
		t.Fatal(err)
	}
	events, done, err := p.ReadEvent(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"model not available"},"id":` + itoa(req.ID) + `}`)
	if err != nil {
		t.Fatalf("error response for a control id must not return err (that is the turn-failure path): %v", err)
	}
	if done {
		t.Error("control error response must not set done")
	}
	if len(events) != 1 || events[0].Type != "control_ack" || events[0].SubType != "error" {
		t.Fatalf("events = %+v, want single control_ack/error", events)
	}
	if !strings.Contains(events[0].Result, "model not available") {
		t.Errorf("Result = %q, want CLI error text", events[0].Result)
	}
	if events[0].RPCRequestID != "req-3" {
		t.Errorf("RPCRequestID = %q, want req-3", events[0].RPCRequestID)
	}
}

// TestClaudeProtocol_WriteSetModel_Wire pins the control_request envelope
// (F6): {"type":"control_request","request_id":…,"request":{"subtype":
// "set_model","model":…}}.
func TestClaudeProtocol_WriteSetModel_Wire(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	var buf bytes.Buffer
	if err := p.WriteSetModel(&buf, "req-11", "opus"); err != nil {
		t.Fatal(err)
	}
	var frame struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype string `json:"subtype"`
			Model   string `json:"model"`
		} `json:"request"`
	}
	if err := json.Unmarshal(buf.Bytes(), &frame); err != nil {
		t.Fatalf("frame not valid JSON: %v\n%s", err, buf.String())
	}
	if frame.Type != "control_request" || frame.Request.Subtype != "set_model" {
		t.Errorf("frame = %+v", frame)
	}
	if frame.RequestID != "req-11" || frame.Request.Model != "opus" {
		t.Errorf("frame = %+v", frame)
	}
}

// TestClaudeProtocol_WriteSetModel_EscapesModel guards the argv/JSON
// boundary: a model string with quotes/backslashes must round-trip as a
// correctly escaped JSON string, not break the envelope.
func TestClaudeProtocol_WriteSetModel_EscapesModel(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	var buf bytes.Buffer
	if err := p.WriteSetModel(&buf, `r"1`, `mo"del\x`); err != nil {
		t.Fatal(err)
	}
	var frame struct {
		RequestID string `json:"request_id"`
		Request   struct {
			Model string `json:"model"`
		} `json:"request"`
	}
	if err := json.Unmarshal(buf.Bytes(), &frame); err != nil {
		t.Fatalf("escaped frame not valid JSON: %v\n%s", err, buf.String())
	}
	if frame.Request.Model != `mo"del\x` || frame.RequestID != `r"1` {
		t.Errorf("round-trip = %+v", frame)
	}
}

// TestCodexProtocol_NoModelSetter pins codex's non-support (NG6): the facet
// assertion must fail so Process.SetModel returns ErrSetModelUnsupported.
func TestCodexProtocol_NoModelSetter(t *testing.T) {
	t.Parallel()
	var p Protocol = &CodexProtocol{}
	if _, ok := p.(ModelSetter); ok {
		t.Fatal("CodexProtocol must not implement ModelSetter (no verified runtime channel, RFC NG6)")
	}
}

// itoa is a tiny local helper so tests don't import strconv for one call.
func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// TestProcess_SetModel_ReturnsOnNaturalExit pins the ack-wait's exit
// condition: a CLI that dies while we await the control_response (EOF /
// cli_exited → readLoop returns → p.done closes) must fail SetModel
// promptly. Only killCh was selected on before, and a natural exit never
// closes killCh — so the dashboard request sat for the full
// setModelAckTimeout (30s) against a process that was already dead.
func TestProcess_SetModel_ReturnsOnNaturalExit(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	go p.readLoop()
	defer p.Kill()

	errCh := make(chan error, 1)
	go func() { errCh <- p.SetModel(context.Background(), "opus") }()

	// Let SetModel get past the Alive() check and park on the ack select,
	// then have the CLI exit naturally (no Kill → killCh stays open).
	time.Sleep(50 * time.Millisecond)
	srv.SendCLIExited(0)

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("SetModel returned nil after the CLI exited; want an error")
		}
		if !strings.Contains(err.Error(), "exited") && !strings.Contains(err.Error(), "terminated") {
			t.Errorf("error should name the process exit, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SetModel still blocked 3s after the CLI exited — ack wait ignores p.done and would sit out the full 30s timeout")
	}
}
