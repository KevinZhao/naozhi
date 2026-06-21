package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// All wire shapes here were captured against codex-cli 0.141.0 on 2026-06-21;
// see docs/rfc/codex-backend-validation.md.

func TestCodexProtocol_Name_Clone(t *testing.T) {
	t.Parallel()
	p := &CodexProtocol{BackendID: "codex"}
	if p.Name() != "codex" {
		t.Errorf("Name() = %q; want codex", p.Name())
	}
	clone, ok := p.Clone().(*CodexProtocol)
	if !ok {
		t.Fatalf("Clone() returned %T; want *CodexProtocol", p.Clone())
	}
	if clone.BackendID != "codex" {
		t.Errorf("Clone lost BackendID = %q; want codex", clone.BackendID)
	}
}

func TestCodexProtocol_BuildArgs(t *testing.T) {
	t.Parallel()
	p := &CodexProtocol{}
	args := p.BuildArgs(SpawnOptions{Model: "openai.gpt-oss-120b"})
	if len(args) == 0 || args[0] != "app-server" {
		t.Fatalf("BuildArgs[0] = %v; want app-server first", args)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"model=openai.gpt-oss-120b",
		"approval_policy=never",
		"sandbox_mode=workspace-write",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("BuildArgs missing %q; got %v", want, args)
		}
	}
	// danger-full-access must NOT be requested (codex 0.141 rejects it with
	// approval_policy=never and falls back to read-only).
	if strings.Contains(joined, "danger-full-access") {
		t.Errorf("BuildArgs must not request danger-full-access; got %v", args)
	}
}

func TestCodexProtocol_BuildArgs_NoModel(t *testing.T) {
	t.Parallel()
	p := &CodexProtocol{}
	args := p.BuildArgs(SpawnOptions{})
	if strings.Contains(strings.Join(args, " "), "model=") {
		t.Errorf("empty Model should not emit model=; got %v", args)
	}
}

func TestCodexProtocol_Init_NewThread(t *testing.T) {
	t.Parallel()
	p := &CodexProtocol{}
	var written bytes.Buffer
	mock := &mockLineReader{lines: []string{
		// initialize (id=0) response
		`{"jsonrpc":"2.0","id":0,"result":{"userAgent":"codex"}}`,
		// thread/start (id=1) response — threadId at result.thread.id
		`{"jsonrpc":"2.0","id":1,"result":{"thread":{"id":"019ee98d-thread"}}}`,
	}}
	rw := &JSONRW{W: &written, R: mock}

	tid, err := p.Init(rw, "", "/tmp/work")
	if err != nil {
		t.Fatalf("Init error: %v", err)
	}
	if tid != "019ee98d-thread" {
		t.Errorf("threadID = %q; want 019ee98d-thread", tid)
	}
	// The handshake must send initialize, then an `initialized` notification
	// (no id), then thread/start.
	out := written.String()
	if !strings.Contains(out, `"method":"initialize"`) {
		t.Error("Init did not send initialize")
	}
	if !strings.Contains(out, `"method":"initialized"`) {
		t.Error("Init did not send initialized notification")
	}
	if !strings.Contains(out, `"method":"thread/start"`) {
		t.Error("Init did not send thread/start")
	}
	// cwd must be threaded into thread/start.
	if !strings.Contains(out, `/tmp/work`) {
		t.Error("Init did not pass cwd into thread/start")
	}
}

func TestCodexProtocol_Init_Resume(t *testing.T) {
	t.Parallel()
	p := &CodexProtocol{}
	var written bytes.Buffer
	mock := &mockLineReader{lines: []string{
		`{"jsonrpc":"2.0","id":0,"result":{}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"thread":{"id":"resumed-thread"}}}`,
	}}
	rw := &JSONRW{W: &written, R: mock}

	tid, err := p.Init(rw, "resumed-thread", "/tmp/work")
	if err != nil {
		t.Fatalf("Init error: %v", err)
	}
	if tid != "resumed-thread" {
		t.Errorf("threadID = %q; want resumed-thread", tid)
	}
	if !strings.Contains(written.String(), `"method":"thread/resume"`) {
		t.Error("Init(resume) did not send thread/resume")
	}
}

func TestCodexProtocol_Init_RPCError(t *testing.T) {
	t.Parallel()
	p := &CodexProtocol{}
	var written bytes.Buffer
	mock := &mockLineReader{lines: []string{
		`{"jsonrpc":"2.0","id":0,"error":{"code":-32600,"message":"Not initialized"}}`,
	}}
	rw := &JSONRW{W: &written, R: mock}
	if _, err := p.Init(rw, "", ""); err == nil {
		t.Fatal("expected error on initialize RPC error")
	}
}

func TestCodexProtocol_WriteMessage_TurnStart(t *testing.T) {
	t.Parallel()
	p := &CodexProtocol{}
	p.storeThreadID("t-1")
	var w bytes.Buffer
	if err := p.WriteMessage(&w, "hello codex", nil); err != nil {
		t.Fatalf("WriteMessage error: %v", err)
	}
	var req struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string `json:"threadId"`
			Input    []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"input"`
		} `json:"params"`
	}
	if err := json.Unmarshal(w.Bytes(), &req); err != nil {
		t.Fatalf("turn/start not valid JSON: %v\n%s", err, w.String())
	}
	if req.Method != "turn/start" {
		t.Errorf("method = %q; want turn/start", req.Method)
	}
	if req.Params.ThreadID != "t-1" {
		t.Errorf("threadId = %q; want t-1", req.Params.ThreadID)
	}
	// input MUST be an array of UserInput (validation §3.2) — a bare string
	// gets rejected with -32600.
	if len(req.Params.Input) != 1 || req.Params.Input[0].Type != "text" ||
		req.Params.Input[0].Text != "hello codex" {
		t.Errorf("input = %+v; want [{text:hello codex}]", req.Params.Input)
	}
}

func TestCodexProtocol_WriteInterrupt(t *testing.T) {
	t.Parallel()
	p := &CodexProtocol{}

	// Pre-handshake: no thread → ErrInterruptUnsupported.
	var w0 bytes.Buffer
	if err := p.WriteInterrupt(&w0, "req-1"); err != ErrInterruptUnsupported {
		t.Errorf("pre-handshake WriteInterrupt err = %v; want ErrInterruptUnsupported", err)
	}

	// Post-handshake: emits turn/interrupt request with threadId.
	p.storeThreadID("t-9")
	var w bytes.Buffer
	if err := p.WriteInterrupt(&w, "req-2"); err != nil {
		t.Fatalf("WriteInterrupt error: %v", err)
	}
	if !strings.Contains(w.String(), `"method":"turn/interrupt"`) {
		t.Errorf("did not emit turn/interrupt; got %s", w.String())
	}
	if !strings.Contains(w.String(), `"threadId":"t-9"`) {
		t.Errorf("turn/interrupt missing threadId; got %s", w.String())
	}
}

func TestCodexProtocol_ReadEvent_AgentMessageDelta(t *testing.T) {
	t.Parallel()
	p := &CodexProtocol{}
	// delta is a plain string (validation §3.3).
	line := `{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"t","itemId":"i","turnId":"u","delta":"Hi"}}`
	evs, done, err := p.ReadEvent(line)
	if err != nil {
		t.Fatalf("ReadEvent err: %v", err)
	}
	if done {
		t.Error("delta should not end the turn")
	}
	if len(evs) != 1 || evs[0].Type != "assistant" {
		t.Fatalf("evs = %+v; want one assistant event", evs)
	}
	// Accumulated text flushes on turn/completed.
	line2 := `{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"t","itemId":"i","turnId":"u","delta":"!"}}`
	if _, _, err := p.ReadEvent(line2); err != nil {
		t.Fatalf("ReadEvent2 err: %v", err)
	}
	doneLine := `{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t","turn":{"status":"completed"}}}`
	evs3, done3, err := p.ReadEvent(doneLine)
	if err != nil {
		t.Fatalf("ReadEvent(turn/completed) err: %v", err)
	}
	if !done3 {
		t.Error("turn/completed should end the turn")
	}
	// Turn boundary emits a synthesised assistant frame (the only source of the
	// dashboard bubble) followed by a result metadata event.
	if len(evs3) != 2 {
		t.Fatalf("turn/completed events = %+v; want 2 (assistant + result)", evs3)
	}
	if evs3[0].Type != "assistant" || evs3[0].Message == nil ||
		len(evs3[0].Message.Content) != 1 || evs3[0].Message.Content[0].Text != "Hi!" {
		t.Errorf("first event = %+v; want assistant frame with text 'Hi!'", evs3[0])
	}
	if evs3[1].Type != "result" || evs3[1].Result != "Hi!" {
		t.Errorf("second event = %+v; want result with text 'Hi!'", evs3[1])
	}
}

func TestCodexProtocol_ReadEvent_TurnCompletedNoText(t *testing.T) {
	t.Parallel()
	p := &CodexProtocol{}
	// No deltas streamed → only a result event, no empty assistant bubble.
	line := `{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t","turn":{"status":"completed"}}}`
	evs, done, err := p.ReadEvent(line)
	if err != nil {
		t.Fatalf("ReadEvent err: %v", err)
	}
	if !done {
		t.Error("turn/completed should end the turn")
	}
	if len(evs) != 1 || evs[0].Type != "result" {
		t.Fatalf("evs = %+v; want single result event when no text streamed", evs)
	}
}

func TestCodexProtocol_ReadEvent_TurnFailed(t *testing.T) {
	t.Parallel()
	p := &CodexProtocol{}
	line := `{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t","turn":{"status":"failed","error":{"message":"namespace tool rejected"}}}}`
	evs, done, err := p.ReadEvent(line)
	if err != nil {
		t.Fatalf("ReadEvent err: %v", err)
	}
	if !done {
		t.Error("failed turn should still end the turn")
	}
	// No text streamed → just the error result event.
	if len(evs) != 1 || evs[len(evs)-1].SubType != "error" {
		t.Fatalf("evs = %+v; want one error result", evs)
	}
	if !strings.Contains(evs[len(evs)-1].Result, "namespace tool rejected") {
		t.Errorf("error text not surfaced: %q", evs[len(evs)-1].Result)
	}
}

func TestCodexProtocol_ReadEvent_TurnFailedKeepsPartialText(t *testing.T) {
	t.Parallel()
	p := &CodexProtocol{}
	// Partial text streamed before a failure must NOT be lost (review #5).
	if _, _, err := p.ReadEvent(`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"t","itemId":"i","turnId":"u","delta":"partial"}}`); err != nil {
		t.Fatalf("delta err: %v", err)
	}
	line := `{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t","turn":{"status":"failed","error":{"message":"boom"}}}}`
	evs, _, err := p.ReadEvent(line)
	if err != nil {
		t.Fatalf("ReadEvent err: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("evs = %+v; want assistant(partial) + error result", evs)
	}
	if evs[0].Type != "assistant" || evs[0].Message == nil || evs[0].Message.Content[0].Text != "partial" {
		t.Errorf("partial text lost: %+v", evs[0])
	}
	if evs[1].SubType != "error" || !strings.Contains(evs[1].Result, "boom") {
		t.Errorf("error result wrong: %+v", evs[1])
	}
}

func TestCodexProtocol_ReadEvent_PostHandshakeErrorResponse(t *testing.T) {
	t.Parallel()
	p := &CodexProtocol{}
	// A deferred turn/start error reply on the main readLoop must close the
	// turn (done=true) and propagate an error, not be silently dropped.
	line := `{"jsonrpc":"2.0","id":3,"error":{"code":-32001,"message":"Server overloaded"}}`
	evs, done, err := p.ReadEvent(line)
	if err == nil {
		t.Fatal("expected error for error-bearing response")
	}
	if !done {
		t.Error("error response should end the turn")
	}
	if len(evs) != 0 {
		t.Errorf("evs = %+v; want none (error surfaced via err)", evs)
	}
}

func TestCodexProtocol_ReadEvent_ToolCall(t *testing.T) {
	t.Parallel()
	p := &CodexProtocol{}
	line := `{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"t","item":{"type":"commandExecution","id":"c1","status":"in_progress"}}}`
	evs, done, err := p.ReadEvent(line)
	if err != nil {
		t.Fatalf("ReadEvent err: %v", err)
	}
	if done {
		t.Error("tool start should not end the turn")
	}
	if len(evs) != 1 || evs[0].SubType != "tool_use" || evs[0].ToolCall == nil {
		t.Fatalf("evs = %+v; want one tool_use event with ToolCall", evs)
	}
	if evs[0].ToolCall.ID != "c1" {
		t.Errorf("ToolCall.ID = %q; want c1", evs[0].ToolCall.ID)
	}
}

func TestCodexProtocol_ReadEvent_TokenUsage(t *testing.T) {
	t.Parallel()
	p := &CodexProtocol{}
	line := `{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"t","turnId":"u","tokenUsage":{"last":{"inputTokens":72,"outputTokens":20,"totalTokens":92},"total":{"totalTokens":92},"modelContextWindow":128000}}}`
	evs, done, err := p.ReadEvent(line)
	if err != nil {
		t.Fatalf("ReadEvent err: %v", err)
	}
	if done {
		t.Error("tokenUsage should not end the turn")
	}
	if len(evs) != 1 || evs[0].Type != "metadata" || evs[0].Metadata == nil {
		t.Fatalf("evs = %+v; want one metadata event", evs)
	}
	if len(evs[0].Metadata.MeteringUsage) != 1 || evs[0].Metadata.MeteringUsage[0].Value != 92 {
		t.Errorf("MeteringUsage = %+v; want one entry value 92", evs[0].Metadata.MeteringUsage)
	}
	if evs[0].Metadata.MeteringUsage[0].Unit != "token" {
		t.Errorf("Unit = %q; want token", evs[0].Metadata.MeteringUsage[0].Unit)
	}
}

func TestCodexProtocol_ReadEvent_UnknownMethodTolerated(t *testing.T) {
	t.Parallel()
	p := &CodexProtocol{}
	for _, line := range []string{
		`{"jsonrpc":"2.0","method":"thread/started","params":{}}`,
		`{"jsonrpc":"2.0","method":"turn/started","params":{}}`,
		`{"jsonrpc":"2.0","method":"some/brand/new/event","params":{"x":1}}`,
		`{"jsonrpc":"2.0","method":"configWarning","params":{}}`,
	} {
		evs, done, err := p.ReadEvent(line)
		if err != nil {
			t.Errorf("ReadEvent(%s) err = %v; want nil", line, err)
		}
		if done {
			t.Errorf("ReadEvent(%s) done = true; want false", line)
		}
		if len(evs) != 0 {
			t.Errorf("ReadEvent(%s) evs = %+v; want none", line, evs)
		}
	}
}

func TestCodexProtocol_ReadEvent_InvalidJSON(t *testing.T) {
	t.Parallel()
	p := &CodexProtocol{}
	if _, _, err := p.ReadEvent(`{not json`); err == nil {
		t.Error("ReadEvent should error on invalid JSON")
	}
}

func TestCodexProtocol_HandleEvent_AutoApprove(t *testing.T) {
	t.Parallel()
	p := &CodexProtocol{}
	// A reverse approval request arrives via ReadEvent as a permission_request.
	reqLine := `{"jsonrpc":"2.0","id":42,"method":"item/commandExecution/requestApproval","params":{"threadId":"t"}}`
	evs, _, err := p.ReadEvent(reqLine)
	if err != nil {
		t.Fatalf("ReadEvent err: %v", err)
	}
	if len(evs) != 1 || evs[0].Type != "permission_request" {
		t.Fatalf("evs = %+v; want one permission_request", evs)
	}
	if evs[0].RPCRequestID != "42" {
		t.Errorf("RPCRequestID = %q; want 42", evs[0].RPCRequestID)
	}

	var w bytes.Buffer
	handled := p.HandleEvent(&w, evs[0])
	if !handled {
		t.Error("HandleEvent should handle permission_request")
	}
	out := w.String()
	if !strings.Contains(out, `"decision":"approved"`) {
		t.Errorf("approval response not approved: %s", out)
	}
	// Numeric id must be echoed unquoted.
	if !strings.Contains(out, `"id":42`) {
		t.Errorf("approval response did not round-trip numeric id: %s", out)
	}
}

func TestCodexProtocol_HandleEvent_NonPermissionPassThrough(t *testing.T) {
	t.Parallel()
	p := &CodexProtocol{}
	var w bytes.Buffer
	if p.HandleEvent(&w, Event{Type: "assistant"}) {
		t.Error("HandleEvent should not handle non-permission events")
	}
	if w.Len() != 0 {
		t.Errorf("HandleEvent wrote %d bytes for non-permission event", w.Len())
	}
}

func TestCodexProtocol_Capabilities(t *testing.T) {
	t.Parallel()
	p := &CodexProtocol{}
	caps := p.Capabilities()
	if caps.Replay || caps.Priority || caps.StreamJSON {
		t.Errorf("caps = %+v; want Replay/Priority/StreamJSON all false", caps)
	}
	if !caps.SoftInterrupt {
		t.Error("codex should advertise SoftInterrupt=true (turn/interrupt)")
	}
	if p.SupportsPriority() || p.SupportsReplay() {
		t.Error("codex SupportsPriority/SupportsReplay must be false in phase1")
	}
}

func TestCodexProtocol_HandleEvent_StringID(t *testing.T) {
	t.Parallel()
	p := &CodexProtocol{}
	// UUID-style ids must be quoted in the response.
	ev := Event{Type: "permission_request", RPCRequestID: "abc-123-uuid"}
	var w bytes.Buffer
	if !p.HandleEvent(&w, ev) {
		t.Fatal("HandleEvent should handle permission_request")
	}
	if !strings.Contains(w.String(), `"id":"abc-123-uuid"`) {
		t.Errorf("string id not quoted: %s", w.String())
	}
}
