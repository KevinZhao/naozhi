package codexjsonl

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSource_LoadBefore_TruncatesLongMessage pins #2327: a long codex
// agent_message must be capped to a 120-rune Summary and a 16000-rune Detail
// (matching the claude path) instead of flowing verbatim to the dashboard,
// which would render an unbounded mega-bubble.
func TestSource_LoadBefore_TruncatesLongMessage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "019ee988-da7f-7821-b6d1-7b74a7db62d8"

	long := strings.Repeat("A", 50000) // well past both caps
	writeRollout(t, dir, sid, []string{
		`{"timestamp":"2026-06-21T09:35:23.000Z","type":"event_msg","payload":{"type":"agent_message","message":"` + long + `"}}`,
	})

	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(got), got)
	}
	e := got[0]
	if n := utf8.RuneCountInString(e.Summary); n > 130 { // 120 + ellipsis
		t.Errorf("Summary not truncated: %d runes", n)
	}
	if n := utf8.RuneCountInString(e.Detail); n > 16010 { // 16000 + ellipsis
		t.Errorf("Detail not truncated: %d runes", n)
	}
	if e.Detail == "" {
		t.Error("Detail empty — should carry the (truncated) full text")
	}
}
