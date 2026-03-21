package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// --- ClaudeProtocol tests ---

func TestClaudeProtocol_Name(t *testing.T) {
	p := &ClaudeProtocol{}
	if p.Name() != "stream-json" {
		t.Errorf("Name() = %q, want stream-json", p.Name())
	}
}

func TestClaudeProtocol_BuildArgs(t *testing.T) {
	p := &ClaudeProtocol{}
	args := p.BuildArgs(SpawnOptions{Model: "opus", ResumeID: "sess_123"})

	found := map[string]bool{}
	for _, a := range args {
		found[a] = true
	}
	for _, want := range []string{"-p", "stream-json", "--verbose", "opus", "sess_123"} {
		if !found[want] {
			t.Errorf("BuildArgs missing %q, got %v", want, args)
		}
	}
}

func TestClaudeProtocol_BuildArgs_NoResume(t *testing.T) {
	p := &ClaudeProtocol{}
	args := p.BuildArgs(SpawnOptions{Model: "sonnet"})
	for _, a := range args {
		if a == "--resume" {
			t.Error("BuildArgs should not include --resume when ResumeID is empty")
		}
	}
}

func TestClaudeProtocol_WriteMessage(t *testing.T) {
	p := &ClaudeProtocol{}
	var buf bytes.Buffer
	if err := p.WriteMessage(&buf, "hello", nil); err != nil {
		t.Fatal(err)
	}
	var msg InputMessage
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type != "user" {
		t.Errorf("Type = %q, want user", msg.Type)
	}
	if s, ok := msg.Message.Content.(string); !ok || s != "hello" {
		t.Errorf("Content = %v, want \"hello\"", msg.Message.Content)
	}
}

func TestClaudeProtocol_ReadEvent_Result(t *testing.T) {
	p := &ClaudeProtocol{}
	line := []byte(`{"type":"result","result":"done","session_id":"s1","total_cost_usd":0.05}`)
	ev, done, err := p.ReadEvent(line)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Error("expected done=true for result event")
	}
	if ev.Result != "done" || ev.SessionID != "s1" {
		t.Errorf("got %+v", ev)
	}
}

func TestClaudeProtocol_ReadEvent_Assistant(t *testing.T) {
	p := &ClaudeProtocol{}
	line := []byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`)
	ev, done, err := p.ReadEvent(line)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Error("expected done=false for assistant event")
	}
	if ev.Type != "assistant" {
		t.Errorf("Type = %q, want assistant", ev.Type)
	}
}

func TestClaudeProtocol_ReadEvent_SkipsHooks(t *testing.T) {
	p := &ClaudeProtocol{}
	for _, sub := range []string{"hook_started", "hook_response"} {
		line := []byte(`{"type":"system","subtype":"` + sub + `"}`)
		ev, done, err := p.ReadEvent(line)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			t.Error("hook events should not be done")
		}
		if ev.Type != "" {
			t.Errorf("hook event should be skipped (zero Event), got Type=%q", ev.Type)
		}
	}
}

func TestClaudeProtocol_ReadEvent_SystemInit(t *testing.T) {
	p := &ClaudeProtocol{}
	line := []byte(`{"type":"system","subtype":"init","session_id":"sess_abc"}`)
	ev, done, err := p.ReadEvent(line)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Error("init should not be done")
	}
	if ev.Type != "system" || ev.SubType != "init" || ev.SessionID != "sess_abc" {
		t.Errorf("got %+v", ev)
	}
}

func TestClaudeProtocol_HandleEvent(t *testing.T) {
	p := &ClaudeProtocol{}
	if p.HandleEvent(nil, Event{Type: "result"}) {
		t.Error("Claude protocol should never handle events internally")
	}
}

func TestClaudeProtocol_Init(t *testing.T) {
	p := &ClaudeProtocol{}
	id, err := p.Init(nil, "")
	if err != nil || id != "" {
		t.Errorf("Init() = (%q, %v), want (\"\", nil)", id, err)
	}
}

// --- ACPProtocol tests ---

func TestACPProtocol_Name(t *testing.T) {
	p := &ACPProtocol{}
	if p.Name() != "acp" {
		t.Errorf("Name() = %q, want acp", p.Name())
	}
}

func TestACPProtocol_BuildArgs(t *testing.T) {
	p := &ACPProtocol{}
	args := p.BuildArgs(SpawnOptions{ExtraArgs: []string{"--debug"}})
	if args[0] != "acp" {
		t.Errorf("first arg should be 'acp', got %q", args[0])
	}
	if args[1] != "--debug" {
		t.Errorf("extra args missing, got %v", args)
	}
}

func TestACPProtocol_WriteMessage(t *testing.T) {
	p := &ACPProtocol{sessionID: "sess_test"}
	var buf bytes.Buffer
	if err := p.WriteMessage(&buf, "hello acp", nil); err != nil {
		t.Fatal(err)
	}
	var req RPCRequest
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &req); err != nil {
		t.Fatal(err)
	}
	if req.Method != "session/prompt" {
		t.Errorf("Method = %q, want session/prompt", req.Method)
	}
	if req.JSONRPC != "2.0" {
		t.Errorf("JSONRPC = %q, want 2.0", req.JSONRPC)
	}
}

func TestACPProtocol_ReadEvent_SessionUpdate_TextChunk(t *testing.T) {
	p := &ACPProtocol{}
	line := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello "}}}}`)
	ev, done, err := p.ReadEvent(line)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Error("text chunk should not be done")
	}
	if ev.Type != "assistant" {
		t.Errorf("Type = %q, want assistant", ev.Type)
	}
	// Verify text accumulated (same goroutine, no readLoop running)
	p.mu.Lock()
	got := p.textBuf
	p.mu.Unlock()
	if got != "hello " {
		t.Errorf("textBuf = %q, want 'hello '", got)
	}

	// Second chunk
	line2 := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"world"}}}}`)
	if _, _, err := p.ReadEvent(line2); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	got = p.textBuf
	p.mu.Unlock()
	if got != "hello world" {
		t.Errorf("textBuf = %q, want 'hello world'", got)
	}
}

func TestACPProtocol_ReadEvent_ToolCall(t *testing.T) {
	p := &ACPProtocol{}
	line := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"c1","title":"Reading file","status":"pending"}}}`)
	ev, done, err := p.ReadEvent(line)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Error("tool_call should not be done")
	}
	if ev.SubType != "tool_use" {
		t.Errorf("SubType = %q, want tool_use", ev.SubType)
	}
}

func TestACPProtocol_ReadEvent_Response_TurnComplete(t *testing.T) {
	p := &ACPProtocol{sessionID: "sess_1"}
	p.mu.Lock()
	p.textBuf = "final answer"
	p.mu.Unlock()

	line := []byte(`{"jsonrpc":"2.0","id":5,"result":{"stopReason":"end_turn"}}`)
	ev, done, err := p.ReadEvent(line)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Error("response should be done=true")
	}
	if ev.Type != "result" {
		t.Errorf("Type = %q, want result", ev.Type)
	}
	if ev.Result != "final answer" {
		t.Errorf("Result = %q, want 'final answer'", ev.Result)
	}
	if ev.SessionID != "sess_1" {
		t.Errorf("SessionID = %q, want sess_1", ev.SessionID)
	}
	// textBuf should be cleared
	if p.textBuf != "" {
		t.Errorf("textBuf should be cleared, got %q", p.textBuf)
	}
}

func TestACPProtocol_ReadEvent_PermissionRequest(t *testing.T) {
	p := &ACPProtocol{}
	line := []byte(`{"jsonrpc":"2.0","id":7,"method":"session/request_permission","params":{"sessionId":"s1","options":[{"optionId":"allow-once","kind":"allow_once"}]}}`)
	ev, done, err := p.ReadEvent(line)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Error("permission request should not be done")
	}
	if ev.Type != "permission_request" {
		t.Errorf("Type = %q, want permission_request", ev.Type)
	}
	if ev.RPCRequestID != 7 {
		t.Errorf("RPCRequestID = %d, want 7", ev.RPCRequestID)
	}
}

func TestACPProtocol_HandleEvent_Permission(t *testing.T) {
	p := &ACPProtocol{}
	var buf bytes.Buffer
	ev := Event{Type: "permission_request", RPCRequestID: 42}
	handled := p.HandleEvent(&buf, ev)
	if !handled {
		t.Error("permission_request should be handled")
	}
	if buf.Len() == 0 {
		t.Error("should have written permission response")
	}
	// Verify response JSON
	var resp map[string]any
	json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &resp)
	if resp["id"] != float64(42) {
		t.Errorf("response id = %v, want 42", resp["id"])
	}
}

func TestACPProtocol_HandleEvent_NonPermission(t *testing.T) {
	p := &ACPProtocol{}
	if p.HandleEvent(nil, Event{Type: "assistant"}) {
		t.Error("non-permission events should not be handled")
	}
}

// --- RPC types tests ---

func TestRPCMessage_IsNotification(t *testing.T) {
	msg := RPCMessage{Method: "session/update"}
	if !msg.IsNotification() {
		t.Error("should be notification")
	}
}

func TestRPCMessage_IsResponse(t *testing.T) {
	id := 1
	msg := RPCMessage{ID: &id, Result: json.RawMessage(`{}`)}
	if !msg.IsResponse() {
		t.Error("should be response")
	}
}

func TestRPCMessage_IsRequest(t *testing.T) {
	id := 1
	msg := RPCMessage{ID: &id, Method: "session/prompt"}
	if !msg.IsRequest() {
		t.Error("should be request")
	}
}

// --- ACP error response test (C2 fix) ---

func TestACPProtocol_ReadEvent_ErrorResponse(t *testing.T) {
	p := &ACPProtocol{sessionID: "sess_1"}
	p.mu.Lock()
	p.textBuf = "partial text"
	p.mu.Unlock()

	line := []byte(`{"jsonrpc":"2.0","id":3,"error":{"code":-32000,"message":"model overloaded"}}`)
	_, done, err := p.ReadEvent(line)
	if done {
		t.Error("error response should not be done=true")
	}
	if err == nil {
		t.Fatal("expected error for RPC error response")
	}
	if !strings.Contains(err.Error(), "model overloaded") {
		t.Errorf("error = %q, should contain 'model overloaded'", err.Error())
	}
}

// --- ACP Init test with mock LineReader (L3 fix) ---

// mockLineReader feeds pre-recorded lines for testing.
type mockLineReader struct {
	lines []string
	idx   int
}

func (m *mockLineReader) ReadLine() ([]byte, bool, error) {
	if m.idx >= len(m.lines) {
		return nil, true, nil
	}
	line := m.lines[m.idx]
	m.idx++
	return []byte(line), false, nil
}

func TestACPProtocol_Init_NewSession(t *testing.T) {
	p := &ACPProtocol{}
	var written bytes.Buffer

	mock := &mockLineReader{lines: []string{
		// Response to initialize (id=0)
		`{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":1,"agentInfo":{"name":"test"}}}`,
		// Response to session/new (id=1)
		`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"sess_new_123"}}`,
	}}
	rw := &JSONRW{W: &written, R: mock}

	sessionID, err := p.Init(rw, "")
	if err != nil {
		t.Fatalf("Init error: %v", err)
	}
	if sessionID != "sess_new_123" {
		t.Errorf("sessionID = %q, want sess_new_123", sessionID)
	}
}

func TestACPProtocol_Init_ResumeSession(t *testing.T) {
	p := &ACPProtocol{}
	var written bytes.Buffer

	mock := &mockLineReader{lines: []string{
		// Response to initialize (id=0)
		`{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":1}}`,
		// Response to session/load (id=1)
		`{"jsonrpc":"2.0","id":1,"result":null}`,
	}}
	rw := &JSONRW{W: &written, R: mock}

	sessionID, err := p.Init(rw, "sess_existing_456")
	if err != nil {
		t.Fatalf("Init error: %v", err)
	}
	if sessionID != "sess_existing_456" {
		t.Errorf("sessionID = %q, want sess_existing_456", sessionID)
	}
}

func TestACPProtocol_Init_RPCError(t *testing.T) {
	p := &ACPProtocol{}
	var written bytes.Buffer

	mock := &mockLineReader{lines: []string{
		`{"jsonrpc":"2.0","id":0,"error":{"code":-32600,"message":"invalid request"}}`,
	}}
	rw := &JSONRW{W: &written, R: mock}

	_, err := p.Init(rw, "")
	if err == nil {
		t.Fatal("expected error for RPC error in init")
	}
	if !strings.Contains(err.Error(), "invalid request") {
		t.Errorf("error = %q, should contain 'invalid request'", err.Error())
	}
}

// --- Existing tests preserved ---

func TestProcessStateString(t *testing.T) {
	tests := []struct {
		state ProcessState
		want  string
	}{
		{StateSpawning, "spawning"},
		{StateReady, "ready"},
		{StateRunning, "running"},
		{StateDead, "dead"},
		{ProcessState(99), "unknown"},
	}

	for _, tt := range tests {
		got := tt.state.String()
		if got != tt.want {
			t.Errorf("ProcessState(%d).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}

func TestNewUserMessage(t *testing.T) {
	msg := NewUserMessage("hello world", nil)
	if msg.Type != "user" {
		t.Errorf("Type = %q, want %q", msg.Type, "user")
	}
	if msg.Message.Role != "user" {
		t.Errorf("Role = %q, want %q", msg.Message.Role, "user")
	}
	if s, ok := msg.Message.Content.(string); !ok || s != "hello world" {
		t.Errorf("Content = %v, want \"hello world\"", msg.Message.Content)
	}
}
