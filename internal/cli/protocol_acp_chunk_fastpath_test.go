package cli

import (
	"strings"
	"testing"
)

// TestACPProtocol_ReadEvent_ChunkFastPath_TextEqualsSlow pins R226-PERF-4 /
// R231-PERF-2: the agent_message_chunk fast path collapses the prior
// 2-step Unmarshal (params → ACPSessionUpdate; Content → ACPTextContent)
// into a single typed Unmarshal into acpChunkParams. The textBuf state and
// the returned Event must be byte-identical to what the slow path produced
// — otherwise streaming replies regress visibly on the dashboard.
func TestACPProtocol_ReadEvent_ChunkFastPath_TextEqualsSlow(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	line := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_abc",` +
		`"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello "}}}}`
	ev, done, err := readOne(t, p, line)
	if err != nil {
		t.Fatalf("ReadEvent: %v", err)
	}
	if done {
		t.Error("chunk should not mark turn done")
	}
	if ev.Type != "assistant" {
		t.Errorf("Type = %q, want assistant", ev.Type)
	}
	if ev.SessionID != "sess_abc" {
		t.Errorf("SessionID = %q, want sess_abc", ev.SessionID)
	}
	p.mu.Lock()
	got := p.textBuf.String()
	p.mu.Unlock()
	if got != "hello " {
		t.Errorf("textBuf = %q, want 'hello '", got)
	}
}

// TestACPProtocol_ReadEvent_ChunkFastPath_UnicodeEscapes verifies the fast
// path delegates UTF-8 / JSON escape decoding to encoding/json (rather than
// hand-rolled byte scanning), so multibyte / escape-laden text round-trips
// correctly into textBuf. This is the regression risk the issue called out:
// a hand-rolled scanner that grabs `"text":"…"` literally would mishandle
// `中`, `\\n`, or embedded `\"`.
func TestACPProtocol_ReadEvent_ChunkFastPath_UnicodeEscapes(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	// "中\nfoo\"bar" exercises a CJK codepoint, a literal newline escape,
	// and an embedded quote — the three cases a naive byte-scanner would
	// trip on.
	line := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1",` +
		`"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"中\nfoo\"bar"}}}}`
	if _, _, err := p.ReadEvent(line); err != nil {
		t.Fatalf("ReadEvent: %v", err)
	}
	p.mu.Lock()
	got := p.textBuf.String()
	p.mu.Unlock()
	want := "中\nfoo\"bar"
	if got != want {
		t.Errorf("textBuf = %q, want %q", got, want)
	}
}

// TestACPProtocol_ReadEvent_ChunkFastPath_FalsePositive ensures the
// bytes.Contains pre-check does not steal frames that merely mention the
// "agent_message_chunk" string in a nested rawInput / tool payload. When
// the SessionUpdate field on the decoded fast-path struct does not match,
// we MUST fall through to the full slow path so tool_call / tool_call_update
// / unknown subtypes still produce their canonical events.
func TestACPProtocol_ReadEvent_ChunkFastPath_FalsePositive(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	// A tool_call frame whose rawInput happens to embed the literal phrase
	// "agent_message_chunk" — the fast-path's bytes.Contains will hit, but
	// fast.Update.SessionUpdate decodes to "tool_call" and we must fall back.
	line := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1",` +
		`"update":{"sessionUpdate":"tool_call","toolCallId":"c1","title":"Search","kind":"search",` +
		`"rawInput":{"query":"explain agent_message_chunk handling"}}}}`
	ev, _, err := readOne(t, p, line)
	if err != nil {
		t.Fatalf("ReadEvent: %v", err)
	}
	if ev.SubType != "tool_use" {
		t.Errorf("SubType = %q, want tool_use (fast-path must fall through to slow path)", ev.SubType)
	}
	if ev.ToolCall == nil || ev.ToolCall.ID != "c1" {
		t.Errorf("ToolCall.ID = %v, want c1", ev.ToolCall)
	}
}

// TestACPProtocol_ReadEvent_ChunkFastPath_OverflowTruncation pins the
// existing buffer-cap behaviour: a chunk whose text would exceed
// maxAssistantMessageContentBytes is truncated at a rune boundary. The
// fast path must preserve this — otherwise a malicious peer streaming
// huge chunks could OOM the process before the turn-complete check fires.
func TestACPProtocol_ReadEvent_ChunkFastPath_OverflowTruncation(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	// Pre-fill textBuf so the next chunk's room is small but non-zero.
	p.textBuf.WriteString(strings.Repeat("a", maxAssistantMessageContentBytes-3))
	// Send a chunk whose text exceeds the remaining 3 bytes. Need to make
	// sure JSON encoding is valid — use a plain ASCII payload longer than 3.
	line := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1",` +
		`"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"OVERFLOWING"}}}}`
	if _, _, err := p.ReadEvent(line); err != nil {
		t.Fatalf("ReadEvent: %v", err)
	}
	p.mu.Lock()
	got := p.textBuf.Len()
	p.mu.Unlock()
	if got > maxAssistantMessageContentBytes {
		t.Errorf("textBuf len = %d, want ≤ %d (cap not enforced on fast path)",
			got, maxAssistantMessageContentBytes)
	}
}
