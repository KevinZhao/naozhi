package kirojsonl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// BenchmarkParseFile_SkipHeavy shows the prefilter + pooled scanner buffer
// keep a tool-heavy session (mostly skipped kinds) cheap: only the handful of
// Prompt/AssistantMessage lines pay for json.Unmarshal, and repeated
// LoadBefore calls reuse the 64 KiB scan buffer instead of re-allocating it.
func BenchmarkParseFile_SkipHeavy(b *testing.B) {
	dir := b.TempDir()
	sid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	lines := []string{promptLine("the only prompt", 1000)}
	noise := `{"version":"v1","kind":"ToolUse","data":{"content":[{"kind":"text","data":"x"}]}}`
	for i := 0; i < 2000; i++ {
		lines = append(lines, noise)
	}
	lines = append(lines, assistantLine("the only answer", 1001))
	path := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		b.Fatal(err)
	}

	s := New(dir, func() string { return sid })
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := s.LoadBefore(ctx, 0, 100)
		if err != nil || len(got) != 2 {
			b.Fatalf("LoadBefore got %d err %v", len(got), err)
		}
	}
}
