package cli

import (
	"encoding/json"
	"testing"
)

// TestShimMsg_LineCarriesInnerStdoutPayload pins the shim wire contract for
// the `line` field of a stdout frame: the inner stream-json event the CLI
// emitted must survive the shim envelope round-trip verbatim and parse
// through Protocol.ReadEvent into the expected Event.
//
// This invariant is load-bearing for #429 (R176-PERF-N2), which proposes
// changing shimMsg.Line from string to json.RawMessage to drop one alloc
// per event. That migration MUST keep two properties intact:
//
//  1. The envelope's `line` field carries the inner event bytes unchanged
//     (no escaping/encoding drift — JSON string vs RawMessage encode the
//     same value on the wire but the Go-side decode differs).
//  2. ReadEvent(msg.Line) parses the carried payload into the same Event.
//
// Locking both now means a string→RawMessage swap that accidentally
// double-encodes or mangles the payload fails CI immediately.
func TestShimMsg_LineCarriesInnerStdoutPayload(t *testing.T) {
	// A representative assistant text frame as the CLI emits it on stdout.
	inner := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello world"}]}}`

	// The shim wraps the inner line as a JSON string in the `line` field.
	// Build the envelope the same way the shim does so the test exercises
	// the real decode path.
	envelope, err := json.Marshal(map[string]any{
		"type": "stdout",
		"seq":  int64(7),
		"line": inner,
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	var msg shimMsg
	if err := json.Unmarshal(envelope, &msg); err != nil {
		t.Fatalf("unmarshal shim envelope: %v", err)
	}

	if msg.Type != "stdout" {
		t.Errorf("Type = %q, want stdout", msg.Type)
	}
	if msg.Seq != 7 {
		t.Errorf("Seq = %d, want 7", msg.Seq)
	}
	// Property 1: the carried payload is byte-identical to the inner line.
	if msg.Line != inner {
		t.Fatalf("Line = %q\nwant %q", msg.Line, inner)
	}

	// Property 2: the carried payload parses through ReadEvent.
	proto := &ClaudeProtocol{}
	events, done, err := proto.ReadEvent(msg.Line)
	if err != nil {
		t.Fatalf("ReadEvent(msg.Line): %v", err)
	}
	if done {
		t.Error("ReadEvent reported done=true for an assistant text frame")
	}
	if len(events) != 1 {
		t.Fatalf("ReadEvent returned %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Type != "assistant" {
		t.Errorf("event Type = %q, want assistant", ev.Type)
	}
	if ev.Message == nil || len(ev.Message.Content) != 1 {
		t.Fatalf("event Message/Content malformed: %+v", ev.Message)
	}
	if got := ev.Message.Content[0].Text; got != "hello world" {
		t.Errorf("content text = %q, want %q", got, "hello world")
	}
}

// TestShimMsg_LineWithEmbeddedQuotesRoundTrips guards the escaping edge the
// string→RawMessage migration is most likely to break: an inner payload
// whose text contains JSON metacharacters (quotes, braces, backslashes)
// must survive the envelope round-trip without corruption.
func TestShimMsg_LineWithEmbeddedQuotesRoundTrips(t *testing.T) {
	inner := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"she said \"hi\" {ok} \\path"}]}}`

	envelope, err := json.Marshal(map[string]any{"type": "stdout", "line": inner})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	var msg shimMsg
	if err := json.Unmarshal(envelope, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Line != inner {
		t.Fatalf("Line corrupted on round-trip:\n got %q\nwant %q", msg.Line, inner)
	}

	proto := &ClaudeProtocol{}
	events, _, err := proto.ReadEvent(msg.Line)
	if err != nil {
		t.Fatalf("ReadEvent: %v", err)
	}
	if len(events) != 1 || events[0].Message == nil || len(events[0].Message.Content) != 1 {
		t.Fatalf("unexpected parse result: %+v", events)
	}
	if got := events[0].Message.Content[0].Text; got != `she said "hi" {ok} \path` {
		t.Errorf("text = %q, want %q", got, `she said "hi" {ok} \path`)
	}
}
