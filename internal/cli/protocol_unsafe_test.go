package cli

import (
	"encoding/json"
	"testing"
)

// TestStringToBytesUnsafe_Empty pins the empty-string contract: nil slice
// (not a zero-length non-nil slice). json.Unmarshal handles a nil input
// the same way it handles "" — it returns an "unexpected end of JSON"
// error rather than panicking on a bad pointer. R222-PERF-3 (#700).
func TestStringToBytesUnsafe_Empty(t *testing.T) {
	got := stringToBytesUnsafe("")
	if got != nil {
		t.Fatalf("expected nil for empty string, got len=%d", len(got))
	}
}

// TestStringToBytesUnsafe_RoundTrip pins that the alias preserves byte-
// for-byte identity with the source string. json.Unmarshal must accept
// the aliased bytes identically to the obvious []byte(line) cast.
func TestStringToBytesUnsafe_RoundTrip(t *testing.T) {
	cases := []string{
		`{}`,
		`{"type":"assistant"}`,
		"hello \xe4\xb8\xad\xe6\x96\x87", // multi-byte UTF-8
		"\x00binary\xff",                 // non-printable
	}
	for _, s := range cases {
		b := stringToBytesUnsafe(s)
		if string(b) != s {
			t.Fatalf("round-trip mismatch: got %q want %q", b, s)
		}
	}
}

// TestClaudeReadEvent_UnsafeAliasParity pins that ReadEvent's unsafe
// alias path produces the same parsed Event as the obvious []byte cast
// would have. Catches regressions if json.Unmarshal ever mutates input.
// R222-PERF-3 (#700).
func TestClaudeReadEvent_UnsafeAliasParity(t *testing.T) {
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`

	p := &ClaudeProtocol{}
	events, done, err := p.ReadEvent(line)
	if err != nil {
		t.Fatalf("ReadEvent err: %v", err)
	}
	if done {
		t.Errorf("done should be false for assistant event")
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != "assistant" {
		t.Errorf("expected type assistant, got %q", events[0].Type)
	}

	// Parity check: the obvious-cast path must produce the same parsed Event.
	var ref Event
	if err := json.Unmarshal([]byte(line), &ref); err != nil {
		t.Fatalf("ref Unmarshal err: %v", err)
	}
	if ref.Type != events[0].Type {
		t.Errorf("type mismatch: got %q want %q", events[0].Type, ref.Type)
	}
}
