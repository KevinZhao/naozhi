package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// readOne is a test helper that flattens Protocol.ReadEvent's []Event return
// into a single Event for tests that expect exactly one semantic event per
// wire frame. Returns a zero Event when ReadEvent skipped the line (length 0
// slice). Multi-event frames (only ACP turn-end today) have their own tests
// that consume the slice directly — using readOne there would silently hide
// the second event.
func readOne(t *testing.T, p Protocol, line string) (Event, bool, error) {
	t.Helper()
	events, done, err := p.ReadEvent(line)
	if err != nil {
		return Event{}, done, err
	}
	if len(events) == 0 {
		return Event{}, done, nil
	}
	if len(events) > 1 {
		t.Fatalf("readOne: expected ≤1 event, got %d (use ReadEvent directly for multi-event frames)", len(events))
	}
	return events[0], done, nil
}

// --- ClaudeProtocol tests ---

func TestClaudeProtocol_Name(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	if p.Name() != "stream-json" {
		t.Errorf("Name() = %q, want stream-json", p.Name())
	}
}

func TestClaudeProtocol_BuildArgs(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	// UUID-shaped ResumeID matches what claude CLI actually emits;
	// resumeIDRe accepts [A-Za-z0-9-]{1,128} (R232-SEC-12).
	resume := "0123abcd-4567-89ef-0123-456789abcdef"
	args := p.BuildArgs(SpawnOptions{Model: "opus", ResumeID: resume})

	found := map[string]bool{}
	for _, a := range args {
		found[a] = true
	}
	for _, want := range []string{"-p", "stream-json", "--verbose", "opus", resume} {
		if !found[want] {
			t.Errorf("BuildArgs missing %q, got %v", want, args)
		}
	}
}

// TestClaudeProtocol_BuildArgs_RejectsBadResumeID locks the regex tightening
// from R232-SEC-12: characters outside [A-Za-z0-9-] silently drop --resume
// rather than passing through to argv.
func TestClaudeProtocol_BuildArgs_RejectsBadResumeID(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	cases := []string{
		"sess_123",               // underscore (legacy form, no longer accepted)
		"sess.123",               // dot
		"id with space",          // whitespace
		strings.Repeat("a", 129), // overlong
	}
	for _, bad := range cases {
		args := p.BuildArgs(SpawnOptions{Model: "sonnet", ResumeID: bad})
		for _, a := range args {
			if a == "--resume" {
				t.Errorf("BuildArgs(%q): emitted --resume despite invalid resume id; args=%v", bad, args)
				break
			}
		}
	}
}

func TestClaudeProtocol_BuildArgs_NoResume(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	args := p.BuildArgs(SpawnOptions{Model: "sonnet"})
	for _, a := range args {
		if a == "--resume" {
			t.Error("BuildArgs should not include --resume when ResumeID is empty")
		}
	}
}

// TestClaudeProtocol_BuildArgs_PermissionModeDefault locks the zero-value
// behaviour for R215-SEC-P1-1 / #531: the legacy --dangerously-skip-permissions
// flag is still emitted when SpawnOptions.PermissionMode is unset, so existing
// callers see no behavioural change. Standard mode (the new opt-in) omits it.
func TestClaudeProtocol_BuildArgs_PermissionModeDefault(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	args := p.BuildArgs(SpawnOptions{Model: "sonnet"})
	got := false
	for _, a := range args {
		if a == "--dangerously-skip-permissions" {
			got = true
			break
		}
	}
	if !got {
		t.Errorf("PermissionModeDefault: expected --dangerously-skip-permissions, got %v", args)
	}
}

func TestClaudeProtocol_BuildArgs_PermissionModeStandard(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	args := p.BuildArgs(SpawnOptions{Model: "sonnet", PermissionMode: PermissionModeStandard})
	for _, a := range args {
		if a == "--dangerously-skip-permissions" {
			t.Errorf("PermissionModeStandard: --dangerously-skip-permissions leaked into argv: %v", args)
		}
	}
}

func TestClaudeProtocol_BuildArgs_DebugFile(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	path := "/data/naozhi/cli-debug/abc123.log"
	args := p.BuildArgs(SpawnOptions{Model: "opus", DebugFile: path})

	// Expect the adjacent pair "--debug api" and "--debug-file <path>".
	var sawDebugAPI, sawDebugFile bool
	for i, a := range args {
		if a == "--debug" && i+1 < len(args) && args[i+1] == "api" {
			sawDebugAPI = true
		}
		if a == "--debug-file" && i+1 < len(args) && args[i+1] == path {
			sawDebugFile = true
		}
	}
	if !sawDebugAPI {
		t.Errorf("DebugFile set: missing `--debug api`, got %v", args)
	}
	if !sawDebugFile {
		t.Errorf("DebugFile set: missing `--debug-file %s`, got %v", path, args)
	}
}

func TestClaudeProtocol_BuildArgs_NoDebugFileByDefault(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	args := p.BuildArgs(SpawnOptions{Model: "opus"}) // DebugFile zero value
	for _, a := range args {
		if a == "--debug" || a == "--debug-file" {
			t.Errorf("zero-value DebugFile leaked debug flags into argv: %v", args)
		}
	}
}

func TestClaudeProtocol_BuildArgs_DebugFileArgvInjectionGuard(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	// A path starting with '-' could be reinterpreted as a flag; reject it.
	args := p.BuildArgs(SpawnOptions{Model: "opus", DebugFile: "-rf"})
	for _, a := range args {
		if a == "--debug-file" || a == "-rf" {
			t.Errorf("DebugFile starting with '-' must be dropped, got %v", args)
		}
	}
}

func TestClaudeProtocol_WriteMessage(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	p := &ClaudeProtocol{}
	line := `{"type":"result","result":"done","session_id":"s1","total_cost_usd":0.05}`
	ev, done, err := readOne(t, p, line)
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
	t.Parallel()
	p := &ClaudeProtocol{}
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`
	ev, done, err := readOne(t, p, line)
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
	t.Parallel()
	p := &ClaudeProtocol{}
	for _, sub := range []string{"hook_started", "hook_response"} {
		line := `{"type":"system","subtype":"` + sub + `"}`
		ev, done, err := readOne(t, p, line)
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
	t.Parallel()
	p := &ClaudeProtocol{}
	line := `{"type":"system","subtype":"init","session_id":"sess_abc"}`
	ev, done, err := readOne(t, p, line)
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
	t.Parallel()
	p := &ClaudeProtocol{}
	if p.HandleEvent(nil, Event{Type: "result"}) {
		t.Error("Claude protocol should never handle events internally")
	}
}

func TestClaudeProtocol_WriteInterrupt(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	p := &ClaudeProtocol{}
	line := `{"type":"control_response","response":{"subtype":"success","request_id":"req-1"}}`
	ev, done, err := readOne(t, p, line)
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

// TestACPProtocol_WriteInterrupt_NoSession covers the pre-handshake case:
// before initialize/session_new completes there is no sessionId to cancel,
// so callers must fall back to SIGINT.
func TestACPProtocol_WriteInterrupt_NoSession(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	var buf bytes.Buffer
	err := p.WriteInterrupt(&buf, "req-1")
	if err == nil {
		t.Fatal("WriteInterrupt without session must return an error")
	}
	if !errors.Is(err, ErrInterruptUnsupported) {
		t.Errorf("err = %v, want ErrInterruptUnsupported", err)
	}
	if buf.Len() != 0 {
		t.Errorf("must not write anything before handshake, got %q", buf.Bytes())
	}
}

// TestACPProtocol_WriteInterrupt_SendsCancelNotification covers the post-
// handshake happy path: a session/cancel notification (no id) is written to
// stdin with the bound sessionId. See V1 in
// docs/rfc/multi-backend-validation.md.
func TestACPProtocol_WriteInterrupt_SendsCancelNotification(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	p.storeSessionID("sess_xyz")

	var buf bytes.Buffer
	err := p.WriteInterrupt(&buf, "ignored-by-acp")
	if err != nil {
		t.Fatalf("WriteInterrupt should succeed with active session, got %v", err)
	}

	out := bytes.TrimSpace(buf.Bytes())
	var msg map[string]any
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if msg["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want 2.0", msg["jsonrpc"])
	}
	if msg["method"] != "session/cancel" {
		t.Errorf("method = %v, want session/cancel", msg["method"])
	}
	// Critical: cancel is a notification, NOT a request — no id field.
	if _, hasID := msg["id"]; hasID {
		t.Error("session/cancel must be a notification (no id field); had id")
	}
	params := msg["params"].(map[string]any)
	if params["sessionId"] != "sess_xyz" {
		t.Errorf("params.sessionId = %v, want sess_xyz", params["sessionId"])
	}
	// Output must be newline-terminated so the agent's line-based reader
	// processes it immediately.
	if !bytes.HasSuffix(buf.Bytes(), []byte("\n")) {
		t.Error("WriteInterrupt output should end with newline for line-based ACP framing")
	}
}

// TestACPProtocol_ReadEvent_CancelledStopReason ensures the cancelled stopReason
// is surfaced as Event.SubType so dispatch can distinguish it from end_turn.
func TestACPProtocol_ReadEvent_CancelledStopReason(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	p.storeSessionID("s1")
	p.mu.Lock()
	p.textBuf.WriteString("partial")
	p.mu.Unlock()

	line := `{"jsonrpc":"2.0","id":3,"result":{"stopReason":"cancelled"}}`
	events, done, err := p.ReadEvent(line)
	if err != nil {
		t.Fatalf("ReadEvent: %v", err)
	}
	if !done {
		t.Error("cancelled response should still mark turn complete")
	}
	// ACP turn-end with buffered text emits two events: the synthesised
	// assistant frame carrying the visible reply, then the result carrying
	// the stopReason + Result for process_send. Both must be present so
	// EventLog renders the bubble (assistant) and Send returns text (result).
	if len(events) != 2 {
		t.Fatalf("want 2 events (assistant text, result), got %d: %+v", len(events), events)
	}
	if events[0].Type != "assistant" {
		t.Errorf("events[0].Type = %q, want assistant", events[0].Type)
	}
	if events[0].Message == nil || len(events[0].Message.Content) != 1 || events[0].Message.Content[0].Text != "partial" {
		t.Errorf("events[0] should carry assistant text=partial, got %+v", events[0].Message)
	}
	if events[1].Type != "result" {
		t.Errorf("events[1].Type = %q, want result", events[1].Type)
	}
	if events[1].SubType != "cancelled" {
		t.Errorf("events[1].SubType = %q, want cancelled (so dispatch can label the turn)", events[1].SubType)
	}
	if events[1].Result != "partial" {
		t.Errorf("partial buffered text should still ride on result.Result for SendResult.Text, got %q", events[1].Result)
	}
}

// TestACPProtocol_ReadEvent_NormalStopReasonRoundTrip ensures end_turn passes
// through too — guards against a copy-paste regression that hardcodes
// "cancelled".
func TestACPProtocol_ReadEvent_NormalStopReasonRoundTrip(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	line := `{"jsonrpc":"2.0","id":5,"result":{"stopReason":"end_turn"}}`
	events, done, err := p.ReadEvent(line)
	if err != nil || !done {
		t.Fatalf("expected done=true err=nil, got done=%v err=%v", done, err)
	}
	// Empty textBuf → no assistant frame; only the result event surfaces.
	if len(events) != 1 || events[0].Type != "result" {
		t.Fatalf("want single result event for empty turn-end, got %+v", events)
	}
	if events[0].SubType != "end_turn" {
		t.Errorf("SubType = %q, want end_turn", events[0].SubType)
	}
}

func TestClaudeProtocol_Init(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	id, err := p.Init(nil, "", "")
	if err != nil || id != "" {
		t.Errorf("Init() = (%q, %v), want (\"\", nil)", id, err)
	}
}

// TestBuildArgs_SettingSourcesUser pins docs/rfc/direct-user-settings.md PR1:
// naozhi-spawned cc loads ~/.claude/settings.json directly, so BuildArgs must
// emit `--setting-sources user` (NOT the historical "") and must NOT inject a
// `--settings <override>` file. On the old code this test fails (the value was
// "" and an override path was appended); on the new code it passes.
func TestBuildArgs_SettingSourcesUser(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	args := p.BuildArgs(SpawnOptions{})

	var foundUser bool
	for i, a := range args {
		if a == "--setting-sources" {
			if i+1 >= len(args) {
				t.Fatalf("--setting-sources has no value: %v", args)
			}
			if args[i+1] != "user" {
				t.Errorf("--setting-sources = %q, want \"user\": %v", args[i+1], args)
			}
			foundUser = true
		}
		if a == "--settings" {
			t.Errorf("BuildArgs must not inject --settings override file: %v", args)
		}
	}
	if !foundUser {
		t.Errorf("--setting-sources missing from argv: %v", args)
	}
}

// TestClaudeProtocol_Clone_Independent verifies Clone returns a usable
// ClaudeProtocol. The struct carries no per-spawn state any more (settings
// plumbing was removed in direct-user-settings PR1) — it is an empty struct,
// so a pointer-identity check is meaningless (Go may give all zero-size
// allocations the same address). We only guard against Clone returning nil or
// the wrong concrete type, and that the clone still builds the user-settings
// argv.
func TestClaudeProtocol_Clone_Independent(t *testing.T) {
	t.Parallel()
	src := &ClaudeProtocol{}
	raw := src.Clone()
	clone, ok := raw.(*ClaudeProtocol)
	if !ok || clone == nil {
		t.Fatalf("Clone returned %T (nil=%v), want non-nil *ClaudeProtocol", raw, clone == nil)
	}
	args := clone.BuildArgs(SpawnOptions{})
	var found bool
	for i, a := range args {
		if a == "--setting-sources" && i+1 < len(args) && args[i+1] == "user" {
			found = true
		}
	}
	if !found {
		t.Errorf("clone BuildArgs missing --setting-sources user: %v", args)
	}
}

// --- ACPProtocol tests ---

func TestACPProtocol_Name(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	if p.Name() != "acp" {
		t.Errorf("Name() = %q, want acp", p.Name())
	}
}

func TestACPProtocol_BuildArgs(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	args := p.BuildArgs(SpawnOptions{ExtraArgs: []string{"--debug"}})
	if args[0] != "acp" {
		t.Errorf("first arg should be 'acp', got %q", args[0])
	}
	if args[1] != "--debug" {
		t.Errorf("extra args missing, got %v", args)
	}
}

// TestACPProtocol_BuildArgs_Model verifies that opts.Model is forwarded as
// `--model <id>` to kiro-cli acp. Without this the cli.backends[].model
// config silently no-ops on kiro (router merge populates SpawnOptions.Model
// but BuildArgs would drop it). Verified on kiro 2.3.0: session/new result
// echoes the flag value via models.currentModelId.
func TestACPProtocol_BuildArgs_Model(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	args := p.BuildArgs(SpawnOptions{Model: "claude-haiku-4.5"})
	// Expect: ["acp", "--model", "claude-haiku-4.5"]
	if len(args) < 3 {
		t.Fatalf("BuildArgs short, got %v", args)
	}
	if args[0] != "acp" {
		t.Errorf("first arg should be 'acp', got %q", args[0])
	}
	var sawFlag, sawValue bool
	for i, a := range args {
		if a == "--model" && i+1 < len(args) && args[i+1] == "claude-haiku-4.5" {
			sawFlag = true
			sawValue = true
			break
		}
	}
	if !sawFlag || !sawValue {
		t.Errorf("BuildArgs missing --model claude-haiku-4.5, got %v", args)
	}
}

// TestACPProtocol_BuildArgs_NoModel verifies that an empty Model produces no
// --model flag in argv. Passing `--model ""` would make kiro reject the
// argv outright, so an absent value must mean "absent flag".
func TestACPProtocol_BuildArgs_NoModel(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	args := p.BuildArgs(SpawnOptions{})
	for _, a := range args {
		if a == "--model" {
			t.Errorf("BuildArgs should not emit --model when Model empty, got %v", args)
		}
	}
}

func TestACPProtocol_WriteMessage(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	p.storeSessionID("sess_test")
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
	t.Parallel()
	p := &ACPProtocol{}
	line := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello "}}}}`
	ev, done, err := readOne(t, p, line)
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
	t.Parallel()
	p := &ACPProtocol{}
	line := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"c1","title":"Reading file","status":"pending"}}}`
	ev, done, err := readOne(t, p, line)
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

// TestACPProtocol_ReadEvent_ToolCall_RichPayload verifies that the
// kind / status / rawInput passthrough survives ReadEvent.
// Multi-Backend RFC §8.3 D17.
func TestACPProtocol_ReadEvent_ToolCall_RichPayload(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	line := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{` +
		`"sessionUpdate":"tool_call","toolCallId":"tooluse_X","title":"Running: cat /etc/hostname",` +
		`"kind":"execute","rawInput":{"command":"cat /etc/hostname"}}}}`
	ev, _, err := readOne(t, p, line)
	if err != nil {
		t.Fatalf("ReadEvent: %v", err)
	}
	if ev.ToolCall == nil {
		t.Fatal("ToolCall should be populated")
	}
	if ev.ToolCall.ID != "tooluse_X" {
		t.Errorf("ID = %q, want tooluse_X", ev.ToolCall.ID)
	}
	if ev.ToolCall.Kind != "execute" {
		t.Errorf("Kind = %q, want execute", ev.ToolCall.Kind)
	}
	if !strings.Contains(ev.ToolCall.InputJSON, "cat /etc/hostname") {
		t.Errorf("InputJSON = %q; want to contain cat /etc/hostname", ev.ToolCall.InputJSON)
	}
	if ev.ToolUseID != "tooluse_X" {
		t.Errorf("ToolUseID = %q, want tooluse_X", ev.ToolUseID)
	}
}

// TestACPProtocol_ReadEvent_ToolCallUpdate_Completed verifies the update
// path carries Status + RawOutput so the dashboard's progress row can
// thread by ID and replace the "pending" pill.
func TestACPProtocol_ReadEvent_ToolCallUpdate_Completed(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	line := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{` +
		`"sessionUpdate":"tool_call_update","toolCallId":"tooluse_X","kind":"execute","status":"completed",` +
		`"title":"Running: cat /etc/hostname","rawOutput":{"items":[{"Json":{"exit_status":"exit status: 0","stdout":"ip-10-0-141-156\n"}}]}}}}`
	ev, done, err := readOne(t, p, line)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Error("tool_call_update should not be done")
	}
	if ev.SubType != "tool_result" {
		t.Errorf("SubType = %q, want tool_result", ev.SubType)
	}
	if ev.ToolCall == nil {
		t.Fatal("ToolCall should be populated")
	}
	if ev.ToolCall.Status != "completed" {
		t.Errorf("Status = %q, want completed", ev.ToolCall.Status)
	}
	if !strings.Contains(ev.ToolCall.OutputJSON, "ip-10-0-141-156") {
		t.Errorf("OutputJSON should contain stdout, got %q", ev.ToolCall.OutputJSON)
	}
}

// TestACPProtocol_ReadEvent_ToolCall_TruncatesLargePayload pins the
// 16K-rune cap on Event.ToolCall.InputJSON / OutputJSON wired up by
// truncateToolJSON. Without this guard a runaway shell tool dumping
// MB-scale stdout would balloon WS frames and slog attrs. Asserts both
// (a) the cap is honoured (output is shorter than the raw payload) and
// (b) the "..." marker is appended so consumers can detect truncation.
// PR #120 review medium follow-up.
func TestACPProtocol_ReadEvent_ToolCall_TruncatesLargePayload(t *testing.T) {
	t.Parallel()
	// 20K runes — comfortably above toolJSONMaxRunes (16K) so the
	// truncation branch fires deterministically.
	const payloadLen = 20000
	bigStdout := strings.Repeat("x", payloadLen)
	line := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{` +
		`"sessionUpdate":"tool_call_update","toolCallId":"tooluse_big","kind":"execute","status":"completed",` +
		`"title":"big tool","rawOutput":{"items":[{"Json":{"stdout":"` + bigStdout + `"}}]}}}}`

	p := &ACPProtocol{}
	ev, _, err := readOne(t, p, line)
	if err != nil {
		t.Fatalf("ReadEvent: %v", err)
	}
	if ev.ToolCall == nil {
		t.Fatal("ToolCall must be populated for tool_call_update")
	}
	got := ev.ToolCall.OutputJSON
	if len(got) >= payloadLen {
		t.Errorf("OutputJSON not truncated: len=%d, want < %d", len(got), payloadLen)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("OutputJSON missing truncation marker '...'; got tail %q", got[max(0, len(got)-20):])
	}
	// Sanity: the prefix must still be the original stdout content.
	if !strings.Contains(got, "stdout") || !strings.Contains(got, "xxxxxxxx") {
		t.Errorf("OutputJSON dropped real content; got %q", got[:min(80, len(got))])
	}
}

func TestACPProtocol_ReadEvent_Response_TurnComplete(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	p.storeSessionID("sess_1")
	p.mu.Lock()
	p.textBuf.WriteString("final answer")
	p.mu.Unlock()

	line := `{"jsonrpc":"2.0","id":5,"result":{"stopReason":"end_turn"}}`
	events, done, err := p.ReadEvent(line)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Error("response should be done=true")
	}
	// Buffered text → two events: assistant text frame + result metadata.
	if len(events) != 2 {
		t.Fatalf("want 2 events (assistant text, result), got %d: %+v", len(events), events)
	}
	if events[0].Type != "assistant" || events[0].Message == nil ||
		len(events[0].Message.Content) != 1 ||
		events[0].Message.Content[0].Text != "final answer" {
		t.Errorf("events[0] should carry assistant text='final answer', got %+v", events[0])
	}
	result := events[1]
	if result.Type != "result" {
		t.Errorf("events[1].Type = %q, want result", result.Type)
	}
	if result.Result != "final answer" {
		t.Errorf("result.Result = %q, want 'final answer' (still rides on the wire for SendResult.Text)", result.Result)
	}
	if result.SessionID != "sess_1" {
		t.Errorf("result.SessionID = %q, want sess_1", result.SessionID)
	}
	// textBuf should be cleared
	if p.textBuf.Len() != 0 {
		t.Errorf("textBuf should be cleared, got %q", p.textBuf.String())
	}
}

func TestACPProtocol_ReadEvent_PermissionRequest(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	line := `{"jsonrpc":"2.0","id":7,"method":"session/request_permission","params":{"sessionId":"s1","options":[{"optionId":"allow_once","kind":"allow_once"}]}}`
	ev, done, err := readOne(t, p, line)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Error("permission request should not be done")
	}
	if ev.Type != "permission_request" {
		t.Errorf("Type = %q, want permission_request", ev.Type)
	}
	if ev.RPCRequestID != "7" {
		t.Errorf("RPCRequestID = %q, want \"7\"", ev.RPCRequestID)
	}
	if len(ev.RawParams) == 0 {
		t.Error("RawParams should be populated for permission_request")
	}
}

// TestACPProtocol_ReadEvent_PermissionRequest_StringID covers kiro 2.3.0's
// UUID-string id form, which broke a *int decoder in the original
// RPCMessage struct. See V8 in docs/rfc/multi-backend-validation.md.
func TestACPProtocol_ReadEvent_PermissionRequest_StringID(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	line := `{"jsonrpc":"2.0","id":"82017692-c404-42d1-9334-ae28dfda0cee","method":"session/request_permission","params":{"sessionId":"s1","options":[{"optionId":"allow_once","kind":"allow_once"}]}}`
	ev, _, err := readOne(t, p, line)
	if err != nil {
		t.Fatalf("kiro UUID id should not fail unmarshal, got: %v", err)
	}
	if ev.Type != "permission_request" {
		t.Errorf("Type = %q, want permission_request", ev.Type)
	}
	if ev.RPCRequestID != "82017692-c404-42d1-9334-ae28dfda0cee" {
		t.Errorf("RPCRequestID = %q, want UUID", ev.RPCRequestID)
	}
}

func TestACPProtocol_HandleEvent_Permission(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	var buf bytes.Buffer
	rawParams := json.RawMessage(`{"sessionId":"s1","toolCall":{"toolCallId":"t1","title":"x"},"options":[{"optionId":"allow_once","name":"Yes","kind":"allow_once"},{"optionId":"reject_once","name":"No","kind":"reject_once"}]}`)
	ev := Event{Type: "permission_request", RPCRequestID: "42", RawParams: rawParams}
	handled := p.HandleEvent(&buf, ev)
	if !handled {
		t.Error("permission_request should be handled")
	}
	if buf.Len() == 0 {
		t.Error("should have written permission response")
	}
	// Verify response JSON. ID round-trips as a JSON number when the
	// agent's id parses as an int (the legacy shape).
	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &resp); err != nil {
		t.Fatalf("response unmarshal: %v", err)
	}
	if resp["id"] != float64(42) {
		t.Errorf("response id = %v, want 42", resp["id"])
	}
	// Outcome must include the optionId picked from the request, not a
	// hardcoded hyphen form.
	result := resp["result"].(map[string]any)
	outcome := result["outcome"].(map[string]any)
	if got := outcome["optionId"]; got != "allow_once" {
		t.Errorf("optionId = %v, want allow_once (read from request)", got)
	}
}

// TestACPProtocol_HandleEvent_Permission_KiroUUID covers the kiro 2.3.0
// shape where id is a UUID string and optionId uses underscores.
func TestACPProtocol_HandleEvent_Permission_KiroUUID(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	var buf bytes.Buffer
	rawParams := json.RawMessage(`{"sessionId":"s1","toolCall":{"toolCallId":"t1","title":"x"},"options":[{"optionId":"allow_once","name":"Yes","kind":"allow_once"},{"optionId":"allow_always","name":"Always","kind":"allow_always"},{"optionId":"reject_once","name":"No","kind":"reject_once"}]}`)
	ev := Event{
		Type:         "permission_request",
		RPCRequestID: "82017692-c404-42d1-9334-ae28dfda0cee",
		RawParams:    rawParams,
	}
	if !p.HandleEvent(&buf, ev) {
		t.Fatal("permission_request should be handled")
	}
	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &resp); err != nil {
		t.Fatalf("response unmarshal: %v", err)
	}
	if resp["id"] != "82017692-c404-42d1-9334-ae28dfda0cee" {
		t.Errorf("response id = %v, want UUID round-trip", resp["id"])
	}
	outcome := resp["result"].(map[string]any)["outcome"].(map[string]any)
	if got := outcome["optionId"]; got != "allow_once" {
		t.Errorf("optionId = %v, want allow_once (kiro form)", got)
	}
}

// TestACPProtocol_HandleEvent_Permission_FallbackOnUnknownOptions ensures
// that even if no allow_* option is present, naozhi still sends a response
// (a stalled permission_request would block the turn forever).
func TestACPProtocol_HandleEvent_Permission_FallbackOnUnknownOptions(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	var buf bytes.Buffer
	rawParams := json.RawMessage(`{"sessionId":"s1","options":[{"optionId":"reject_once","name":"No","kind":"reject_once"}]}`)
	ev := Event{Type: "permission_request", RPCRequestID: "1", RawParams: rawParams}
	if !p.HandleEvent(&buf, ev) {
		t.Fatal("must always handle permission_request, even on unknown options")
	}
	if buf.Len() == 0 {
		t.Error("must always send a response, even on unknown options")
	}
}

func TestACPProtocol_HandleEvent_NonPermission(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	if p.HandleEvent(nil, Event{Type: "assistant"}) {
		t.Error("non-permission events should not be handled")
	}
}

// --- RPC types tests ---

func TestRPCMessage_IsNotification(t *testing.T) {
	t.Parallel()
	msg := RPCMessage{Method: "session/update"}
	if !msg.IsNotification() {
		t.Error("should be notification (no id, has method)")
	}
	// Explicit JSON null is also "no id" per JSON-RPC.
	msgNull := RPCMessage{ID: json.RawMessage(`null`), Method: "session/update"}
	if !msgNull.IsNotification() {
		t.Error("null id + method should be notification")
	}
}

func TestRPCMessage_IsResponse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
	}{
		{"int id", `{"jsonrpc":"2.0","id":1,"result":{}}`},
		{"string id (kiro UUID)", `{"jsonrpc":"2.0","id":"82017692-c404-42d1-9334-ae28dfda0cee","result":{}}`},
		{"zero int id (legal but rare)", `{"jsonrpc":"2.0","id":0,"result":{}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var msg RPCMessage
			if err := json.Unmarshal([]byte(c.raw), &msg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !msg.IsResponse() {
				t.Errorf("%q should be response", c.raw)
			}
		})
	}
}

func TestRPCMessage_IsRequest(t *testing.T) {
	t.Parallel()
	raw := `{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{}}`
	var msg RPCMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !msg.IsRequest() {
		t.Error("should be request")
	}
}

// TestRPCMessage_IDAsInt covers the dual-shape decoder: int passes through,
// string returns ok=false, missing returns ok=false. See V8 in
// docs/rfc/multi-backend-validation.md.
func TestRPCMessage_IDAsInt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw    string
		want   int
		wantOK bool
	}{
		{`{"jsonrpc":"2.0","id":7,"result":{}}`, 7, true},
		{`{"jsonrpc":"2.0","id":0,"result":{}}`, 0, true},
		{`{"jsonrpc":"2.0","id":"82017692-c404-42d1-9334-ae28dfda0cee","method":"x"}`, 0, false},
		{`{"jsonrpc":"2.0","method":"notif"}`, 0, false},
		{`{"jsonrpc":"2.0","id":null,"method":"notif"}`, 0, false},
	}
	for _, c := range cases {
		var msg RPCMessage
		if err := json.Unmarshal([]byte(c.raw), &msg); err != nil {
			t.Fatalf("unmarshal %q: %v", c.raw, err)
		}
		got, ok := msg.IDAsInt()
		if ok != c.wantOK || got != c.want {
			t.Errorf("IDAsInt(%q) = (%d, %v); want (%d, %v)", c.raw, got, ok, c.want, c.wantOK)
		}
	}
}

// TestRPCMessage_IDAsString covers stringification: int -> "n", string ->
// itself, missing/null -> ("", false).
func TestRPCMessage_IDAsString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw    string
		want   string
		wantOK bool
	}{
		{`{"jsonrpc":"2.0","id":7,"result":{}}`, "7", true},
		{`{"jsonrpc":"2.0","id":"abc","result":{}}`, "abc", true},
		{`{"jsonrpc":"2.0","id":"82017692-c404-42d1-9334-ae28dfda0cee","method":"x"}`, "82017692-c404-42d1-9334-ae28dfda0cee", true},
		{`{"jsonrpc":"2.0","method":"notif"}`, "", false},
		{`{"jsonrpc":"2.0","id":null,"method":"notif"}`, "", false},
	}
	for _, c := range cases {
		var msg RPCMessage
		if err := json.Unmarshal([]byte(c.raw), &msg); err != nil {
			t.Fatalf("unmarshal %q: %v", c.raw, err)
		}
		got, ok := msg.IDAsString()
		if ok != c.wantOK || got != c.want {
			t.Errorf("IDAsString(%q) = (%q, %v); want (%q, %v)", c.raw, got, ok, c.want, c.wantOK)
		}
	}
}

// TestRPCMessage_NoUnmarshalErrorOnStringID ensures the master-broken case
// (string id triggering "cannot unmarshal string into Go struct field
// RPCMessage.id of type int") does not regress.
func TestRPCMessage_NoUnmarshalErrorOnStringID(t *testing.T) {
	t.Parallel()
	raw := `{"jsonrpc":"2.0","id":"82017692-c404-42d1-9334-ae28dfda0cee","method":"session/request_permission","params":{}}`
	var msg RPCMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("kiro UUID id triggered unmarshal error (regression of V8): %v", err)
	}
	if !msg.IsRequest() {
		t.Error("should classify as request")
	}
}

// --- ACP _kiro.dev/metadata normalize tests (Sprint 4) ---

// TestACPProtocol_ReadEvent_KiroMetadata_FullPayload ensures the synthetic
// Type:"metadata" Event carries normalized fields from the kiro 2.3.0
// _kiro.dev/metadata notification (V10 sample).
func TestACPProtocol_ReadEvent_KiroMetadata_FullPayload(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	line := `{"jsonrpc":"2.0","method":"_kiro.dev/metadata","params":{"sessionId":"s1","contextUsagePercentage":0.0285,"turnDurationMs":2000,"meteringUsage":[{"value":0.024,"unit":"credit","unitPlural":"credits"}]}}`
	ev, done, err := readOne(t, p, line)
	if err != nil {
		t.Fatalf("ReadEvent: %v", err)
	}
	if done {
		t.Error("metadata frame should not mark turn complete")
	}
	if ev.Type != "metadata" {
		t.Fatalf("Type = %q, want metadata", ev.Type)
	}
	if ev.SessionID != "s1" {
		t.Errorf("SessionID = %q, want s1", ev.SessionID)
	}
	if ev.Metadata == nil {
		t.Fatal("Metadata should be populated")
	}
	// kiro reports 0-1 fraction; we scale to 0-100 in parseKiroMetadata so
	// dashboard doesn't have to remember.
	if got := ev.Metadata.ContextUsagePercent; got < 2.84 || got > 2.86 {
		t.Errorf("ContextUsagePercent = %v, want ~2.85", got)
	}
	if ev.Metadata.TurnDurationMs != 2000 {
		t.Errorf("TurnDurationMs = %d, want 2000", ev.Metadata.TurnDurationMs)
	}
	if len(ev.Metadata.MeteringUsage) != 1 {
		t.Fatalf("MeteringUsage len = %d, want 1", len(ev.Metadata.MeteringUsage))
	}
	m := ev.Metadata.MeteringUsage[0]
	if m.Value != 0.024 || m.Unit != "credit" || m.UnitPlural != "credits" {
		t.Errorf("MeteringEntry = %+v, want value=0.024 unit=credit", m)
	}
}

// TestACPProtocol_ReadEvent_KiroMetadata_MissingFields tolerates partial
// payloads — kiro may omit metering on idle turns; downstream consumers
// already treat zero as "no signal".
func TestACPProtocol_ReadEvent_KiroMetadata_MissingFields(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	line := `{"jsonrpc":"2.0","method":"_kiro.dev/metadata","params":{"sessionId":"s1","contextUsagePercentage":0.5}}`
	ev, _, err := readOne(t, p, line)
	if err != nil {
		t.Fatalf("ReadEvent: %v", err)
	}
	if ev.Metadata == nil {
		t.Fatal("Metadata should be populated even with partial payload")
	}
	if ev.Metadata.ContextUsagePercent != 50 {
		t.Errorf("ContextUsagePercent = %v, want 50", ev.Metadata.ContextUsagePercent)
	}
	if ev.Metadata.TurnDurationMs != 0 {
		t.Errorf("TurnDurationMs = %d, want 0 for missing field", ev.Metadata.TurnDurationMs)
	}
	if ev.Metadata.MeteringUsage != nil {
		t.Errorf("MeteringUsage should be nil for missing field, got %v", ev.Metadata.MeteringUsage)
	}
}

// TestNormalizeContextUsage pins the dual-format + clamp contract for kiro's
// contextUsagePercentage. Live deployment (PR #126 follow-up) found values
// > 1.0 (kiro lets the counter run past 100% on overflow), so the helper
// must accept BOTH 0-1 fractions AND already-percent inputs and clamp to
// the dashboard-visible [0, 100] range.
func TestNormalizeContextUsage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   float64
		want float64
	}{
		{"zero", 0, 0},
		{"fraction small", 0.0285, 2.85},
		{"fraction half", 0.5, 50},
		{"fraction one", 1.0, 100},
		{"already percent typical", 75, 75},
		{"already percent overflow", 116.789, 100},
		{"negative noise", -0.1, 0},
		{"huge garbage", 1e9, 100},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeContextUsage(tc.in)
			// Use abs-diff for the fractional comparison (float math).
			diff := got - tc.want
			if diff < 0 {
				diff = -diff
			}
			if diff > 1e-9 {
				t.Errorf("normalizeContextUsage(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestACPProtocol_ReadEvent_KiroMetadata_SchemaDriftSwallowed locks the
// schema-drift contract: a malformed _kiro.dev/metadata frame returns the
// "skip this line" zero-Event without aborting readLoop.
func TestACPProtocol_ReadEvent_KiroMetadata_SchemaDriftSwallowed(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	// params is an array instead of an object — clearly a schema drift.
	line := `{"jsonrpc":"2.0","method":"_kiro.dev/metadata","params":[1,2,3]}`
	ev, done, err := readOne(t, p, line)
	if err != nil {
		t.Errorf("schema drift should not surface as readLoop error, got %v", err)
	}
	if done {
		t.Error("schema drift should not mark turn complete")
	}
	if ev.Type != "" {
		t.Errorf("schema drift should return zero Event, got Type=%q", ev.Type)
	}
}

// TestProcess_ApplyMetadata_AndAccessors covers the lock-free getter/setter
// pair on Process. Accessors return zeros until applyMetadata fires.
func TestProcess_ApplyMetadata_AndAccessors(t *testing.T) {
	t.Parallel()
	p := &Process{}
	if got := p.ContextUsagePercent(); got != 0 {
		t.Errorf("zero-value ContextUsagePercent = %v, want 0", got)
	}
	if got := p.TurnDurationMs(); got != 0 {
		t.Errorf("zero-value TurnDurationMs = %v, want 0", got)
	}
	if got := p.MeteringUsage(); got != nil {
		t.Errorf("zero-value MeteringUsage = %v, want nil", got)
	}

	// UI Round 5 R5-4: per-unit accumulation. Two entries with the same
	// Unit ("credit") within ONE applyMetadata call collapse into a single
	// summed entry (0.01 + 0.02 = 0.03). Different units stay separate.
	p.applyMetadata(&EventMetadata{
		ContextUsagePercent: 42.5,
		TurnDurationMs:      1500,
		MeteringUsage: []MeteringEntry{
			{Value: 0.01, Unit: "credit", UnitPlural: "credits"},
			{Value: 0.02, Unit: "credit", UnitPlural: "credits"},
		},
	})
	if got := p.ContextUsagePercent(); got != 42.5 {
		t.Errorf("ContextUsagePercent = %v, want 42.5", got)
	}
	if got := p.TurnDurationMs(); got != 1500 {
		t.Errorf("TurnDurationMs = %v, want 1500", got)
	}
	usage := p.MeteringUsage()
	if len(usage) != 1 {
		t.Fatalf("MeteringUsage len = %d, want 1 (same-unit merge): %+v", len(usage), usage)
	}
	// Tolerate the float 0.01+0.02 ≠ 0.03 drift; assert close-enough.
	if diff := usage[0].Value - 0.03; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("MeteringUsage[0].Value = %v, want ~0.03 (sum of 0.01 + 0.02)", usage[0].Value)
	}

	// Defensive copy — mutating the returned slice must not affect the
	// next reader.
	usage[0].Value = 99
	if again := p.MeteringUsage(); again[0].Value > 0.04 {
		t.Errorf("MeteringUsage returned shared slice; got mutated %v on second read", again)
	}

	// Second applyMetadata call adds another turn's metering. Session-
	// level total must accumulate: 0.03 (turn 1) + 0.05 (turn 2) = 0.08.
	p.applyMetadata(&EventMetadata{
		MeteringUsage: []MeteringEntry{
			{Value: 0.05, Unit: "credit", UnitPlural: "credits"},
		},
	})
	usage2 := p.MeteringUsage()
	if len(usage2) != 1 {
		t.Fatalf("session-level metering len = %d, want 1: %+v", len(usage2), usage2)
	}
	if diff := usage2[0].Value - 0.08; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("session credits = %v, want ~0.08 (0.03 + 0.05)", usage2[0].Value)
	}

	// nil applyMetadata must not panic / mutate state.
	p.applyMetadata(nil)
	if p.ContextUsagePercent() != 42.5 {
		t.Error("nil applyMetadata should be a no-op")
	}

	// Zero-fields applyMetadata must not regress prior values.
	p.applyMetadata(&EventMetadata{})
	if p.ContextUsagePercent() != 42.5 {
		t.Error("zero-field applyMetadata should not overwrite existing percent")
	}
	if p.TurnDurationMs() != 1500 {
		t.Error("zero-field applyMetadata should not overwrite existing duration")
	}
}

// TestProcess_MeteringUsage_FastPath pins the R227-PERF-10 invariant: when
// no metering rows exist the atomic length probe lets MeteringUsage skip
// the RLock entirely. After applyMetadata fires, meteringLen tracks
// len(meteringUsage) so the fast-path stays accurate.
func TestProcess_MeteringUsage_FastPath(t *testing.T) {
	t.Parallel()
	p := &Process{}
	// Zero-value: meteringLen is 0, MeteringUsage returns nil without
	// touching meteringMu (verified by the absence of races in -race).
	if got := p.meteringLen.Load(); got != 0 {
		t.Errorf("zero-value meteringLen = %d, want 0", got)
	}
	if got := p.MeteringUsage(); got != nil {
		t.Errorf("zero-value MeteringUsage = %v, want nil", got)
	}

	// After the first applyMetadata, meteringLen must mirror the slice
	// length so the fast-path no longer short-circuits.
	p.applyMetadata(&EventMetadata{
		MeteringUsage: []MeteringEntry{
			{Value: 0.01, Unit: "credit", UnitPlural: "credits"},
		},
	})
	if got := p.meteringLen.Load(); got != 1 {
		t.Errorf("post-apply meteringLen = %d, want 1", got)
	}
	if got := p.MeteringUsage(); len(got) != 1 {
		t.Errorf("post-apply MeteringUsage len = %d, want 1", len(got))
	}

	// Second call adds a different unit — meteringLen must reflect the
	// post-merge slice length (not the input batch length).
	p.applyMetadata(&EventMetadata{
		MeteringUsage: []MeteringEntry{
			{Value: 100, Unit: "token", UnitPlural: "tokens"},
		},
	})
	if got := p.meteringLen.Load(); got != 2 {
		t.Errorf("post-second-apply meteringLen = %d, want 2", got)
	}
	if got := p.MeteringUsage(); len(got) != 2 {
		t.Errorf("post-second-apply MeteringUsage len = %d, want 2", len(got))
	}

	// Same-unit merge keeps slice length stable; meteringLen stays 2.
	p.applyMetadata(&EventMetadata{
		MeteringUsage: []MeteringEntry{
			{Value: 0.02, Unit: "credit", UnitPlural: "credits"},
		},
	})
	if got := p.meteringLen.Load(); got != 2 {
		t.Errorf("same-unit-merge meteringLen = %d, want 2 (no growth)", got)
	}
}

// --- ACP error response test (C2 fix; updated 2026-05-19) ---

// TestACPProtocol_ReadEvent_ErrorResponse pins the post-fix contract:
// an RPC error response to session/prompt closes the turn (done=true) so
// readLoop can synthesize a result event and unblock state=running.
//
// Earlier code had done=false here, which made every kiro internal-error
// silently strand the session — readLoop would log "skip unparseable event"
// and the dashboard would never see a turn end (operator-visible as "kiro
// session never replies" until manual interrupt or restart).
func TestACPProtocol_ReadEvent_ErrorResponse(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	p.storeSessionID("sess_1")
	p.mu.Lock()
	p.textBuf.WriteString("partial text")
	p.mu.Unlock()

	line := `{"jsonrpc":"2.0","id":3,"error":{"code":-32000,"message":"model overloaded"}}`
	_, done, err := readOne(t, p, line)
	if !done {
		t.Error("error response MUST be done=true so the turn unblocks state=running")
	}
	if err == nil {
		t.Fatal("expected error for RPC error response")
	}
	if !errors.Is(err, ErrACPRPC) {
		t.Errorf("err should wrap ErrACPRPC, got %v", err)
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
	t.Parallel()
	p := &ACPProtocol{}
	var written bytes.Buffer

	mock := &mockLineReader{lines: []string{
		// Response to initialize (id=0)
		`{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":1,"agentInfo":{"name":"test"}}}`,
		// Response to session/new (id=1)
		`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"sess_new_123"}}`,
	}}
	rw := &JSONRW{W: &written, R: mock}

	sessionID, err := p.Init(rw, "", "")
	if err != nil {
		t.Fatalf("Init error: %v", err)
	}
	if sessionID != "sess_new_123" {
		t.Errorf("sessionID = %q, want sess_new_123", sessionID)
	}
}

func TestACPProtocol_Init_ResumeSession(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	var written bytes.Buffer

	mock := &mockLineReader{lines: []string{
		// Response to initialize (id=0)
		`{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":1}}`,
		// Response to session/load (id=1)
		`{"jsonrpc":"2.0","id":1,"result":null}`,
	}}
	rw := &JSONRW{W: &written, R: mock}

	sessionID, err := p.Init(rw, "sess_existing_456", "")
	if err != nil {
		t.Fatalf("Init error: %v", err)
	}
	if sessionID != "sess_existing_456" {
		t.Errorf("sessionID = %q, want sess_existing_456", sessionID)
	}
}

func TestACPProtocol_Init_RPCError(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	var written bytes.Buffer

	mock := &mockLineReader{lines: []string{
		`{"jsonrpc":"2.0","id":0,"error":{"code":-32600,"message":"invalid request"}}`,
	}}
	rw := &JSONRW{W: &written, R: mock}

	_, err := p.Init(rw, "", "")
	if err == nil {
		t.Fatal("expected error for RPC error in init")
	}
	if !strings.Contains(err.Error(), "invalid request") {
		t.Errorf("error = %q, should contain 'invalid request'", err.Error())
	}
}

// --- Existing tests preserved ---

func TestProcessStateString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		state ProcessState
		want  string
	}{
		{StateSpawning, "running"},
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
	t.Parallel()
	msg := NewUserMessageWithMeta("hello world", nil, "", "")
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
	t.Parallel()
	images := []ImageData{
		{Data: []byte("fake-png-data"), MimeType: "image/png"},
		{Data: []byte("fake-jpeg-data"), MimeType: "image/jpeg"},
	}
	msg := NewUserMessageWithMeta("describe this", images, "", "")

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
	t.Parallel()
	images := []ImageData{{Data: []byte("img"), MimeType: "image/png"}}
	msg := NewUserMessageWithMeta("", images, "", "")

	blocks, ok := msg.Message.Content.([]any)
	if !ok {
		t.Fatalf("Content type = %T, want []any", msg.Message.Content)
	}
	// Only 1 image block, no text block when text is empty
	if len(blocks) != 1 {
		t.Fatalf("len(blocks) = %d, want 1", len(blocks))
	}
}

// TestNewUserMessage_WithFileRef_PrependsHint verifies the core of the PDF
// support: a KindFileRef attachment must NOT produce a content block, but
// it MUST prepend a Read-tool instruction to the text so Claude opens the
// workspace file on its own.
func TestNewUserMessage_WithFileRef_PrependsHint(t *testing.T) {
	t.Parallel()
	atts := []Attachment{{
		Kind:          KindFileRef,
		MimeType:      "application/pdf",
		WorkspacePath: ".naozhi/attachments/2026-05-06/deadbeef.pdf",
		OrigName:      "report.pdf",
		Size:          1024 * 1024,
	}}
	msg := NewUserMessageWithMeta("summarize key points", atts, "", "")

	// Text-only message: the Content field must be a plain string (no
	// multimodal block array) because we have no inline bytes. This keeps
	// the wire form identical to a legacy text-only send plus a prefix.
	s, ok := msg.Message.Content.(string)
	if !ok {
		t.Fatalf("Content type = %T, want string (file_ref alone)", msg.Message.Content)
	}
	if !strings.Contains(s, ".naozhi/attachments/2026-05-06/deadbeef.pdf") {
		t.Errorf("text missing workspace path: %q", s)
	}
	if !strings.Contains(s, "Read tool") {
		t.Errorf("text missing Read-tool hint: %q", s)
	}
	if !strings.Contains(s, "report.pdf") {
		t.Errorf("text missing original filename: %q", s)
	}
	if !strings.Contains(s, "summarize key points") {
		t.Errorf("user text dropped: %q", s)
	}
	// User text must appear AFTER the hint — the model needs the
	// instruction before the question so the Read call happens first.
	hintIdx := strings.Index(s, "Read tool")
	userIdx := strings.Index(s, "summarize key points")
	if hintIdx == -1 || userIdx == -1 || hintIdx >= userIdx {
		t.Errorf("hint should precede user text, got hint@%d user@%d in %q", hintIdx, userIdx, s)
	}
}

// TestNewUserMessage_WithImageAndFileRef_Mixed covers the realistic case
// where a user attaches both a PDF and a screenshot. Inline images still
// produce image blocks; file_refs still go into the text prefix.
func TestNewUserMessage_WithImageAndFileRef_Mixed(t *testing.T) {
	t.Parallel()
	atts := []Attachment{
		{Kind: KindImageInline, Data: []byte("png"), MimeType: "image/png"},
		{Kind: KindFileRef, MimeType: "application/pdf",
			WorkspacePath: ".naozhi/attachments/x/y.pdf", OrigName: "y.pdf"},
	}
	msg := NewUserMessageWithMeta("compare these", atts, "", "")

	blocks, ok := msg.Message.Content.([]any)
	if !ok {
		t.Fatalf("Content type = %T, want []any", msg.Message.Content)
	}
	// 1 image block + 1 text block (text carries the hint and user text
	// together; the file_ref does NOT produce its own block).
	if len(blocks) != 2 {
		t.Fatalf("len(blocks) = %d, want 2", len(blocks))
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, `"type":"image"`) {
		t.Error("missing image block in wire form")
	}
	if !strings.Contains(got, `"type":"text"`) {
		t.Error("missing text block in wire form")
	}
	if !strings.Contains(got, "y.pdf") {
		t.Error("file_ref path absent from text block")
	}
	// The PDF bytes must NOT appear in the wire form — the whole point of
	// file_ref is to avoid forwarding the payload over stdin.
	if strings.Contains(got, "application/pdf") {
		t.Error("file_ref should not emit application/pdf in content blocks")
	}
}

// TestNewUserMessage_NoFileRef_ByteIdentical pins the no-regression
// invariant: when no file_ref is present, the wire form must be bit-for-bit
// identical to the pre-PDF code path. Otherwise every image-only send in
// production would subtly change behaviour on rollout.
func TestNewUserMessage_NoFileRef_ByteIdentical(t *testing.T) {
	t.Parallel()
	// Text-only
	text := NewUserMessageWithMeta("just text", nil, "", "")
	s, ok := text.Message.Content.(string)
	if !ok || s != "just text" {
		t.Errorf("text-only content changed: got %T %v", text.Message.Content, text.Message.Content)
	}

	// Image-only
	imgs := []Attachment{{Kind: KindImageInline, Data: []byte("x"), MimeType: "image/png"}}
	m := NewUserMessageWithMeta("look", imgs, "", "")
	blocks, _ := m.Message.Content.([]any)
	if len(blocks) != 2 {
		t.Fatalf("image+text expected 2 blocks, got %d", len(blocks))
	}
	// The text block must carry the user text verbatim, no hint prefix.
	last, _ := blocks[len(blocks)-1].(inputTextBlock)
	if last.Text != "look" {
		t.Errorf("image-only text block leaked hint prefix: %q", last.Text)
	}
}

// TestNewUserMessage_FileRefWithoutPath_Skipped covers a caller bug:
// WorkspacePath must be populated before calling NewUserMessageWithMeta.
// A missing path means the persistence layer didn't run; we silently drop
// the bullet rather than confuse the model with an empty "- \n" line.
func TestNewUserMessage_FileRefWithoutPath_Skipped(t *testing.T) {
	t.Parallel()
	atts := []Attachment{{
		Kind:     KindFileRef,
		MimeType: "application/pdf",
		// WorkspacePath intentionally empty
		OrigName: "orphan.pdf",
	}}
	msg := NewUserMessageWithMeta("hello", atts, "", "")
	s, _ := msg.Message.Content.(string)
	if strings.Contains(s, "  - \n") {
		t.Errorf("empty path should be skipped, got hint: %q", s)
	}
}

func TestClaudeProtocol_WriteMessage_WithImages(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	p := &ACPProtocol{}
	p.storeSessionID("sess_img")
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

// TestFilterDeniedFlags pins the R219-SEC-1 / #653 ExtraArgs hardening: the
// known-dangerous flag surface (--mcp-config / --add-dir /
// --dangerously-skip-permissions / --append-system-prompt and friends) MUST
// be stripped before BuildArgs hands ExtraArgs to the spawn argv. Bare,
// equals-form, and bare-with-attached-value paths all exercised so the
// filter cannot be bypassed by trivial argv shape variation.
func TestFilterDeniedFlags(t *testing.T) {
	t.Parallel()
	t.Run("nothing_filtered_returns_input_unchanged", func(t *testing.T) {
		t.Parallel()
		in := []string{"--debug", "--keep"}
		out := filterDeniedFlags(in)
		if len(out) != 2 || out[0] != "--debug" || out[1] != "--keep" {
			t.Errorf("expected pass-through, got %v", out)
		}
	})
	t.Run("strip_bare_flag_with_value", func(t *testing.T) {
		t.Parallel()
		in := []string{"--debug", "--mcp-config", "/tmp/bad.json", "--keep"}
		out := filterDeniedFlags(in)
		want := []string{"--debug", "--keep"}
		if !equalSlice(out, want) {
			t.Errorf("got %v, want %v", out, want)
		}
	})
	t.Run("strip_equals_form", func(t *testing.T) {
		t.Parallel()
		in := []string{"--debug", "--mcp-config=/tmp/bad.json", "--keep"}
		out := filterDeniedFlags(in)
		want := []string{"--debug", "--keep"}
		if !equalSlice(out, want) {
			t.Errorf("got %v, want %v", out, want)
		}
	})
	t.Run("strip_protocol_framing_flags", func(t *testing.T) {
		t.Parallel()
		// R100110-INJ-1: --output-format / --input-format / --verbose /
		// --replay-user-messages are BuildArgs-owned protocol-framing flags;
		// an operator override would break the stream-json NDJSON parser.
		// Bare-with-value and equals forms must both be stripped.
		in := []string{
			"--output-format", "json",
			"--input-format=text",
			"--verbose",
			"--replay-user-messages",
			"--keep",
		}
		out := filterDeniedFlags(in)
		want := []string{"--keep"}
		if !equalSlice(out, want) {
			t.Errorf("got %v, want %v", out, want)
		}
	})
	t.Run("strip_skip_permissions_no_value", func(t *testing.T) {
		t.Parallel()
		// --dangerously-skip-permissions takes no value; the next flag
		// must NOT be swallowed as its value.
		in := []string{"--dangerously-skip-permissions", "--debug"}
		out := filterDeniedFlags(in)
		want := []string{"--debug"}
		if !equalSlice(out, want) {
			t.Errorf("got %v, want %v", out, want)
		}
	})
	t.Run("strip_multiple_denied", func(t *testing.T) {
		t.Parallel()
		in := []string{
			"--add-dir", "/etc",
			"--append-system-prompt", "you are evil",
			"--allowed-tools", "Bash",
			"--keep", "--this",
		}
		out := filterDeniedFlags(in)
		want := []string{"--keep", "--this"}
		if !equalSlice(out, want) {
			t.Errorf("got %v, want %v", out, want)
		}
	})
	t.Run("non_flag_token_passes_through", func(t *testing.T) {
		t.Parallel()
		in := []string{"plain-arg", "--debug"}
		out := filterDeniedFlags(in)
		want := []string{"plain-arg", "--debug"}
		if !equalSlice(out, want) {
			t.Errorf("got %v, want %v", out, want)
		}
	})
}

// TestBuildArgs_StripsDeniedExtraArgs is an end-to-end check via the public
// BuildArgs entry point: a caller cannot inject --mcp-config (or the other
// denied flags) into argv even when feeding ExtraArgs straight through.
func TestBuildArgs_StripsDeniedExtraArgs(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	args := p.BuildArgs(SpawnOptions{
		ExtraArgs: []string{"--mcp-config", "/tmp/evil.json", "--debug"},
	})
	for _, a := range args {
		if a == "--mcp-config" || a == "/tmp/evil.json" {
			t.Errorf("denied token leaked into argv: %v", args)
		}
	}
	// --debug should still be present (denylist, not allowlist).
	saw := false
	for _, a := range args {
		if a == "--debug" {
			saw = true
			break
		}
	}
	if !saw {
		t.Errorf("--debug should pass through, got %v", args)
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
