package kirojsonl

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSource_LoadBefore_TruncatesLongMessage pins #2327: a long kiro
// AssistantMessage must be capped to a 120-rune Summary and a 16000-rune
// Detail (matching the claude path) instead of flowing verbatim to the
// dashboard, which would render an unbounded mega-bubble.
func TestSource_LoadBefore_TruncatesLongMessage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "kiro-session-truncate"

	long := strings.Repeat("A", 50000) // well past both caps
	writeSession(t, dir, sid, []string{
		promptLine("hi", 1_700_000_000),
		assistantLine(long, 1_700_000_001),
	})

	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	var asst *struct {
		Summary, Detail string
	}
	for i := range got {
		if got[i].Type == "text" {
			asst = &struct{ Summary, Detail string }{got[i].Summary, got[i].Detail}
		}
	}
	if asst == nil {
		t.Fatalf("no assistant text entry returned: %+v", got)
	}
	if n := utf8.RuneCountInString(asst.Summary); n > 130 { // 120 + ellipsis
		t.Errorf("Summary not truncated: %d runes", n)
	}
	if n := utf8.RuneCountInString(asst.Detail); n > 16010 { // 16000 + ellipsis
		t.Errorf("Detail not truncated: %d runes", n)
	}
	if asst.Detail == "" {
		t.Error("Detail empty — should carry the (truncated) full text")
	}
}
