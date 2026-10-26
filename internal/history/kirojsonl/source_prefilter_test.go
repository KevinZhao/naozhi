package kirojsonl

import (
	"context"
	"testing"
)

// TestParseFile_PrefilterEquivalence pins #2246: the byte quick-filter that
// skips non-Prompt/AssistantMessage lines before json.Unmarshal must not
// change which entries are produced. A session mixing target records with
// skipped kinds (tool_use, system) and decoy lines whose only "kind" token is
// a content-chunk kind ("text", "thinking") must yield exactly the Prompt +
// AssistantMessage entries, in order.
func TestParseFile_PrefilterEquivalence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "11111111-2222-3333-4444-555555555555"

	lines := []string{
		promptLine("first prompt", 1000),
		// A skipped kind that nonetheless embeds a content chunk whose kind is
		// "text" — must NOT be mistaken for a target record by the prefilter.
		`{"version":"v1","kind":"ToolUse","data":{"content":[{"kind":"text","data":"tool noise"}]}}`,
		assistantLine("first answer", 1001),
		// A bare system record with no content — skipped.
		`{"version":"v1","kind":"SystemMessage","data":{"content":[]}}`,
		promptLine("second prompt", 2000),
		assistantLine("second answer", 2001),
	}
	writeSession(t, dir, sid, lines)

	s := New(dir, func() string { return sid })
	got, err := s.LoadBefore(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}

	want := []struct {
		typ string
		sum string
	}{
		{"user", "first prompt"},
		{"text", "first answer"},
		{"user", "second prompt"},
		{"text", "second answer"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Type != w.typ || got[i].Summary != w.sum {
			t.Errorf("entry %d = {%q,%q}, want {%q,%q}", i, got[i].Type, got[i].Summary, w.typ, w.sum)
		}
	}
}
