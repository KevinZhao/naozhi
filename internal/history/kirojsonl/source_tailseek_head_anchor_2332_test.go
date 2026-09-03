package kirojsonl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseFile_TailSeekAnchorsAssistantsFromDroppedHead pins #2332: in a
// >maxFileBytes kiro session the tail window can begin long after the most
// recent Prompt (real sessions here have a single Prompt followed by ~100 MiB
// of ToolResults/AssistantMessage). AssistantMessage records carry no
// meta.timestamp and borrow the last Prompt's ts; when that Prompt lives in
// the discarded head, every in-window assistant was dropped and LoadBefore
// returned nothing at all. parseFile must scan the head backwards for the
// anchoring Prompt and seed lastPromptMS from it.
func TestParseFile_TailSeekAnchorsAssistantsFromDroppedHead(t *testing.T) {
	t.Parallel()
	const anchorTS int64 = 1_700_000_000

	var sb strings.Builder
	// The only Prompt lives at offset 0 — far outside the tail window and
	// many backscan chunks away from the seek point.
	sb.WriteString(promptLine("ANCHOR prompt in dropped head", anchorTS) + "\n")
	// ToolResults filler carries no Prompt/Assistant marker; 4 KiB lines
	// guarantee lines straddle the reverse-scan chunk boundaries.
	filler := fmt.Sprintf(
		`{"version":"v1","kind":"ToolResults","data":{"message_id":"tr","content":[{"kind":"toolResult","data":{"content":[{"kind":"text","data":%q}]}}]}}`+"\n",
		strings.Repeat("x", 4096),
	)
	for sb.Len() < (17 << 20) {
		sb.WriteString(filler)
	}
	// Decoy: a ToolResults record whose payload embeds the Prompt marker
	// text with a different timestamp. The backscan must reject it (kind
	// is not Prompt) and keep looking for the real anchor.
	sb.WriteString(`{"version":"v1","kind":"ToolResults","data":{"message_id":"decoy","content":[{"kind":"text","data":"{\"kind\":\"Prompt\",\"meta\":{\"timestamp\":1799999999}}"}]}}` + "\n")
	// In-window orphan assistants: no meta.timestamp, no Prompt ahead of them.
	for i := 1; i <= 3; i++ {
		sb.WriteString(fmt.Sprintf(
			`{"version":"v1","kind":"AssistantMessage","data":{"message_id":"a-%d","content":[{"kind":"text","data":"ORPHAN-%d"}]}}`+"\n", i, i))
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "head-anchor.jsonl")
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	s := New(dir, func() string { return "head-anchor" })
	got := s.parseFile(context.Background(), f, 0)

	var orphans []string
	for _, e := range got {
		if strings.Contains(e.Summary, "ANCHOR") {
			t.Errorf("head Prompt outside the window leaked in as an entry: %+v", e)
		}
		if strings.HasPrefix(e.Summary, "ORPHAN-") {
			orphans = append(orphans, e.Summary)
			want := anchorTS*1000 + int64(len(orphans))
			if e.Type != "text" || e.Time != want {
				t.Errorf("%s: type=%q time=%d; want text anchored at %d", e.Summary, e.Type, e.Time, want)
			}
		}
	}
	if len(orphans) != 3 {
		t.Fatalf("#2332: in-window assistants anchored to a head Prompt were dropped; got %v, want 3", orphans)
	}
}
