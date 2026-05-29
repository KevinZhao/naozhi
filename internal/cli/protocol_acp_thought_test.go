package cli

import "testing"

// TestACPProtocol_ThoughtChunk_AccumulatesAndFlushes verifies agent_thought_chunk
// notifications accumulate into thoughtBuf (producing no per-chunk EventLog
// entry) and flush as a single "thinking" block at the turn boundary, ahead of
// the assistant text frame. Regression guard for the bug where thought chunks
// fell through to the default branch and rendered as empty "agent_thought_chunk"
// system rows.
func TestACPProtocol_ThoughtChunk_AccumulatesAndFlushes(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}
	p.storeSessionID("s1")

	chunk := func(kind, text string) string {
		return `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1",` +
			`"update":{"sessionUpdate":"` + kind + `","content":{"type":"text","text":"` + text + `"}}}}`
	}

	// Two thought chunks + one message chunk: none should emit a visible entry.
	for _, line := range []string{
		chunk("agent_thought_chunk", "let me "),
		chunk("agent_thought_chunk", "think"),
		chunk("agent_message_chunk", "hi"),
	} {
		ev, done, err := readOne(t, p, line)
		if err != nil {
			t.Fatalf("ReadEvent(%s): %v", line, err)
		}
		if done {
			t.Error("chunk should not end turn")
		}
		if ev.Message != nil {
			t.Errorf("chunk produced a visible message: %+v", ev.Message)
		}
	}

	// Turn-end response flushes thinking THEN text.
	events, done, err := p.ReadEvent(`{"jsonrpc":"2.0","id":0,"result":{"stopReason":"end_turn"}}`)
	if err != nil {
		t.Fatalf("ReadEvent(response): %v", err)
	}
	if !done {
		t.Error("response should end turn")
	}
	if len(events) != 3 {
		t.Fatalf("want 3 events (thinking, text, result), got %d: %+v", len(events), events)
	}
	if events[0].Message == nil || len(events[0].Message.Content) != 1 ||
		events[0].Message.Content[0].Type != "thinking" ||
		events[0].Message.Content[0].Text != "let me think" {
		t.Errorf("event[0] not the expected thinking block: %+v", events[0])
	}
	if events[1].Message == nil || events[1].Message.Content[0].Type != "text" ||
		events[1].Message.Content[0].Text != "hi" {
		t.Errorf("event[1] not the expected text block: %+v", events[1])
	}
	if events[2].Type != "result" {
		t.Errorf("event[2] type = %q, want result", events[2].Type)
	}
}
