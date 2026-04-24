package cli

import (
	"bytes"
	"encoding/json"
	"errors"
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
	line := `{"type":"result","result":"done","session_id":"s1","total_cost_usd":0.05}`
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
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`
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
		line := `{"type":"system","subtype":"` + sub + `"}`
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
	line := `{"type":"system","subtype":"init","session_id":"sess_abc"}`
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

func TestClaudeProtocol_WriteInterrupt(t *testing.T) {
	p := &ClaudeProtocol{}
	var buf bytes.Buffer
	if err := p.WriteInterrupt(&buf, "req-42"); err != nil {
		t.Fatal(err)
	}
	// Must be a single NDJSON line (trailing '\n' only, no embedded newlines
	// in the payload — shim enforces this framing).
	out := buf.Bytes()
	if len(out) == 0 || out[len(out)-1] != '\n' {
		t.Fatalf("output missing trailing newline: %q", out)
	}
	if bytes.Count(out, []byte{'\n'}) != 1 {
		t.Fatalf("output must be a single NDJSON line, got %q", out)
	}
	var parsed struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype string `json:"subtype"`
		} `json:"request"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &parsed); err != nil {
		t.Fatalf("unmarshal: %v (raw=%q)", err, out)
	}
	if parsed.Type != "control_request" {
		t.Errorf("Type = %q, want control_request", parsed.Type)
	}
	if parsed.RequestID != "req-42" {
		t.Errorf("RequestID = %q, want req-42", parsed.RequestID)
	}
	if parsed.Request.Subtype != "interrupt" {
		t.Errorf("Request.Subtype = %q, want interrupt", parsed.Request.Subtype)
	}
}

func TestClaudeProtocol_ReadEvent_SkipsControlResponse(t *testing.T) {
	p := &ClaudeProtocol{}
	line := `{"type":"control_response","response":{"subtype":"success","request_id":"req-1"}}`
	ev, done, err := p.ReadEvent(line)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Error("control_response must not complete a turn")
	}
	if ev.Type != "" {
		t.Errorf("control_response should be skipped, got Type=%q", ev.Type)
	}
}

func TestACPProtocol_WriteInterrupt_Unsupported(t *testing.T) {
	p := &ACPProtocol{}
	var buf bytes.Buffer
	err := p.WriteInterrupt(&buf, "req-1")
	if err == nil {
		t.Fatal("ACP WriteInterrupt must return an error")
	}
	if !errors.Is(err, ErrInterruptUnsupported) {
		t.Errorf("err = %v, want ErrInterruptUnsupported", err)
	}
	if buf.Len() != 0 {
		t.Errorf("ACP WriteInterrupt must not write anything, got %q", buf.Bytes())
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
	line := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello "}}}}`
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
	got := p.textBuf.String()
	p.mu.Unlock()
	if got != "hello " {
		t.Errorf("textBuf = %q, want 'hello '", got)
	}

	// Second chunk
	line2 := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"world"}}}}`
	if _, _, err := p.ReadEvent(line2); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	got = p.textBuf.String()
	p.mu.Unlock()
	if got != "hello world" {
		t.Errorf("textBuf = %q, want 'hello world'", got)
	}
}

func TestACPProtocol_ReadEvent_ToolCall(t *testing.T) {
	p := &ACPProtocol{}
	line := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"c1","title":"Reading file","status":"pending"}}}`
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
	p.textBuf.WriteString("final answer")
	p.mu.Unlock()

	line := `{"jsonrpc":"2.0","id":5,"result":{"stopReason":"end_turn"}}`
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
	if p.textBuf.Len() != 0 {
		t.Errorf("textBuf should be cleared, got %q", p.textBuf.String())
	}
}

func TestACPProtocol_ReadEvent_PermissionRequest(t *testing.T) {
	p := &ACPProtocol{}
	line := `{"jsonrpc":"2.0","id":7,"method":"session/request_permission","params":{"sessionId":"s1","options":[{"optionId":"allow-once","kind":"allow_once"}]}}`
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
	p.textBuf.WriteString("partial text")
	p.mu.Unlock()

	line := `{"jsonrpc":"2.0","id":3,"error":{"code":-32000,"message":"model overloaded"}}`
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
		{StateSpawning, "running"},
		{StateReady, "ready"},
		{StateRunning, "running"},
		{StateDead, "ready"},
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

func TestNewUserMessage_WithImages(t *testing.T) {
	images := []ImageData{
		{Data: []byte("fake-png-data"), MimeType: "image/png"},
		{Data: []byte("fake-jpeg-data"), MimeType: "image/jpeg"},
	}
	msg := NewUserMessage("describe this", images)

	if msg.Type != "user" || msg.Message.Role != "user" {
		t.Fatalf("unexpected type/role: %q / %q", msg.Type, msg.Message.Role)
	}

	// Content must be a slice, not a string
	blocks, ok := msg.Message.Content.([]any)
	if !ok {
		t.Fatalf("Content type = %T, want []any", msg.Message.Content)
	}
	// 2 images + 1 text block
	if len(blocks) != 3 {
		t.Fatalf("len(blocks) = %d, want 3", len(blocks))
	}

	// Marshal and re-parse to verify JSON structure
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	json.Unmarshal(data, &raw)
	var inner map[string]json.RawMessage
	json.Unmarshal(raw["message"], &inner)

	var contentBlocks []map[string]any
	if err := json.Unmarshal(inner["content"], &contentBlocks); err != nil {
		t.Fatalf("failed to parse content blocks: %v", err)
	}

	// First two blocks should be images
	for i := 0; i < 2; i++ {
		if contentBlocks[i]["type"] != "image" {
			t.Errorf("block[%d].type = %v, want image", i, contentBlocks[i]["type"])
		}
		src, _ := contentBlocks[i]["source"].(map[string]any)
		if src["type"] != "base64" {
			t.Errorf("block[%d].source.type = %v, want base64", i, src["type"])
		}
		if src["data"] == nil || src["data"] == "" {
			t.Errorf("block[%d].source.data is empty", i)
		}
	}
	// Verify first image media_type
	src0, _ := contentBlocks[0]["source"].(map[string]any)
	if src0["media_type"] != "image/png" {
		t.Errorf("block[0].source.media_type = %v, want image/png", src0["media_type"])
	}
	// Verify second image media_type
	src1, _ := contentBlocks[1]["source"].(map[string]any)
	if src1["media_type"] != "image/jpeg" {
		t.Errorf("block[1].source.media_type = %v, want image/jpeg", src1["media_type"])
	}

	// Last block should be text
	if contentBlocks[2]["type"] != "text" {
		t.Errorf("block[2].type = %v, want text", contentBlocks[2]["type"])
	}
	if contentBlocks[2]["text"] != "describe this" {
		t.Errorf("block[2].text = %v, want 'describe this'", contentBlocks[2]["text"])
	}
}

func TestNewUserMessage_WithImages_EmptyText(t *testing.T) {
	images := []ImageData{{Data: []byte("img"), MimeType: "image/png"}}
	msg := NewUserMessage("", images)

	blocks, ok := msg.Message.Content.([]any)
	if !ok {
		t.Fatalf("Content type = %T, want []any", msg.Message.Content)
	}
	// Only 1 image block, no text block when text is empty
	if len(blocks) != 1 {
		t.Fatalf("len(blocks) = %d, want 1", len(blocks))
	}
}

func TestClaudeProtocol_WriteMessage_WithImages(t *testing.T) {
	p := &ClaudeProtocol{}
	var buf bytes.Buffer
	images := []ImageData{
		{Data: []byte("png-bytes"), MimeType: "image/png"},
	}
	if err := p.WriteMessage(&buf, "what is this?", images); err != nil {
		t.Fatal(err)
	}

	// Parse the NDJSON line
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &parsed); err != nil {
		t.Fatal(err)
	}

	var inner map[string]json.RawMessage
	json.Unmarshal(parsed["message"], &inner)

	var contentBlocks []map[string]any
	if err := json.Unmarshal(inner["content"], &contentBlocks); err != nil {
		t.Fatalf("content should be array for multimodal, got error: %v", err)
	}
	if len(contentBlocks) != 2 {
		t.Fatalf("len(contentBlocks) = %d, want 2 (1 image + 1 text)", len(contentBlocks))
	}
	if contentBlocks[0]["type"] != "image" {
		t.Errorf("block[0].type = %v, want image", contentBlocks[0]["type"])
	}
	if contentBlocks[1]["type"] != "text" {
		t.Errorf("block[1].type = %v, want text", contentBlocks[1]["type"])
	}
}

func TestACPProtocol_WriteMessage_WithImages(t *testing.T) {
	p := &ACPProtocol{sessionID: "sess_img"}
	var buf bytes.Buffer
	images := []ImageData{
		{Data: []byte("jpeg-bytes"), MimeType: "image/jpeg"},
	}
	if err := p.WriteMessage(&buf, "analyze", images); err != nil {
		t.Fatal(err)
	}

	var req map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &req); err != nil {
		t.Fatal(err)
	}
	if string(req["method"]) != `"session/prompt"` {
		t.Errorf("method = %s, want session/prompt", req["method"])
	}

	var params map[string]json.RawMessage
	json.Unmarshal(req["params"], &params)

	var sessionID string
	json.Unmarshal(params["sessionId"], &sessionID)
	if sessionID != "sess_img" {
		t.Errorf("sessionId = %q, want sess_img", sessionID)
	}

	var prompt []map[string]any
	if err := json.Unmarshal(params["prompt"], &prompt); err != nil {
		t.Fatalf("prompt parse error: %v", err)
	}
	if len(prompt) != 2 {
		t.Fatalf("len(prompt) = %d, want 2 (1 image + 1 text)", len(prompt))
	}
	if prompt[0]["type"] != "image" {
		t.Errorf("prompt[0].type = %v, want image", prompt[0]["type"])
	}
	src, _ := prompt[0]["source"].(map[string]any)
	if src["media_type"] != "image/jpeg" {
		t.Errorf("prompt[0].source.media_type = %v, want image/jpeg", src["media_type"])
	}
	if prompt[1]["type"] != "text" {
		t.Errorf("prompt[1].type = %v, want text", prompt[1]["type"])
	}
	if prompt[1]["text"] != "analyze" {
		t.Errorf("prompt[1].text = %v, want analyze", prompt[1]["text"])
	}
}
