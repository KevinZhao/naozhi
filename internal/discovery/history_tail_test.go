package discovery

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// LoadHistoryTail
// ---------------------------------------------------------------------------

func TestLoadHistoryTail_LimitHonored(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/tail-limit"
	sessionID := "00000000-0000-0000-0000-000000001001"
	dirName := projDirName(cwd)

	// 50 user lines numbered 0-49; tail(10) should give us indices 40-49.
	lines := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		lines = append(lines, userJSONLLine("user", fmt.Sprintf("msg-%02d", i)))
	}
	makeSessionJSONL(t, claudeDir, dirName, sessionID, lines)

	entries, err := LoadHistoryTail(claudeDir, sessionID, cwd, 10)
	if err != nil {
		t.Fatalf("LoadHistoryTail error: %v", err)
	}
	if len(entries) != 10 {
		t.Fatalf("expected 10 entries, got %d", len(entries))
	}
	// Chronological order → first should be msg-40, last should be msg-49.
	if entries[0].Summary != "msg-40" {
		t.Errorf("entries[0].Summary = %q, want msg-40", entries[0].Summary)
	}
	if entries[9].Summary != "msg-49" {
		t.Errorf("entries[9].Summary = %q, want msg-49", entries[9].Summary)
	}
}

func TestLoadHistoryTail_LimitLargerThanFile(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/tail-small"
	sessionID := "00000000-0000-0000-0000-000000001002"
	dirName := projDirName(cwd)

	lines := []string{
		userJSONLLine("user", "only-one"),
	}
	makeSessionJSONL(t, claudeDir, dirName, sessionID, lines)

	entries, err := LoadHistoryTail(claudeDir, sessionID, cwd, 100)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Summary != "only-one" {
		t.Errorf("summary = %q", entries[0].Summary)
	}
}

// TestLoadHistoryTail_SpanningChunks forces a single line to straddle the
// 256KB tail chunk boundary and verifies the carry-over logic reassembles it.
func TestLoadHistoryTail_SpanningChunks(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/tail-span"
	sessionID := "00000000-0000-0000-0000-000000001003"
	dirName := projDirName(cwd)

	// Build a big line that is larger than the chunk size so the reverse
	// reader must reassemble it across at least two chunks.
	big := strings.Repeat("x", tailChunkSize+1024)
	lines := []string{
		userJSONLLine("user", "first"),
		userJSONLLine("user", big),
		userJSONLLine("user", "last"),
	}
	makeSessionJSONL(t, claudeDir, dirName, sessionID, lines)

	entries, err := LoadHistoryTail(claudeDir, sessionID, cwd, 10)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[0].Summary != "first" {
		t.Errorf("entries[0].Summary = %q, want first", entries[0].Summary)
	}
	if entries[2].Summary != "last" {
		t.Errorf("entries[2].Summary = %q, want last", entries[2].Summary)
	}
	// The middle entry is the big line; Summary is truncated to 120 runes so
	// just ensure it is non-empty rather than asserting exact content.
	if entries[1].Summary == "" {
		t.Error("big middle entry summary is empty")
	}
}

func TestLoadHistoryTail_LimitZeroFallsBack(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/tail-fallback"
	sessionID := "00000000-0000-0000-0000-000000001004"
	dirName := projDirName(cwd)

	lines := []string{
		userJSONLLine("user", "a"),
		userJSONLLine("user", "b"),
	}
	makeSessionJSONL(t, claudeDir, dirName, sessionID, lines)

	// limit <= 0 must delegate to the legacy LoadHistory implementation.
	entries, err := LoadHistoryTail(claudeDir, sessionID, cwd, 0)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (legacy path), got %d", len(entries))
	}
}

func TestLoadHistoryTail_MissingFile(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	entries, err := LoadHistoryTail(claudeDir, "does-not-exist", "", 50)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if entries != nil {
		t.Errorf("expected nil entries for missing file, got %v", entries)
	}
}

func TestLoadHistoryTail_SkipsMalformed(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/tail-malformed"
	sessionID := "00000000-0000-0000-0000-000000001005"
	dirName := projDirName(cwd)

	lines := []string{
		"not json at all",
		userJSONLLine("user", "survivor"),
		`{"type":"user","timestamp":"x","message":{}}`, // missing content
	}
	makeSessionJSONL(t, claudeDir, dirName, sessionID, lines)

	entries, err := LoadHistoryTail(claudeDir, sessionID, cwd, 10)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (only survivor), got %d", len(entries))
	}
	if entries[0].Summary != "survivor" {
		t.Errorf("summary = %q, want survivor", entries[0].Summary)
	}
}

func TestLoadHistoryTail_CancelledCtx(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/tail-cancel"
	sessionID := "00000000-0000-0000-0000-000000001006"
	dirName := projDirName(cwd)

	lines := []string{userJSONLLine("user", "should-not-matter")}
	makeSessionJSONL(t, claudeDir, dirName, sessionID, lines)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := LoadHistoryTailCtx(ctx, claudeDir, sessionID, cwd, 10)
	if err == nil {
		t.Fatal("expected error from cancelled ctx, got nil")
	}
}

// ---------------------------------------------------------------------------
// LoadHistoryChainTail
// ---------------------------------------------------------------------------

func TestLoadHistoryChainTail_StopsAtBudget(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/chain"
	dirName := projDirName(cwd)

	// Three sessions in the chain. Chain order stored is oldest → newest.
	// Each session has 10 user entries. Session IDs must be UUID-shaped
	// since resolveJSONLPath now rejects non-UUID IDs (R54 defence).
	type entry struct{ id, tag string }
	rows := []entry{
		{"11111111-1111-1111-1111-111111111111", "chain-a"},
		{"22222222-2222-2222-2222-222222222222", "chain-b"},
		{"33333333-3333-3333-3333-333333333333", "chain-c"},
	}
	ids := make([]string, len(rows))
	for idx, row := range rows {
		ids[idx] = row.id
		lines := make([]string, 0, 10)
		for i := 0; i < 10; i++ {
			lines = append(lines, userJSONLLine("user", fmt.Sprintf("%s-%d", row.tag, i)))
		}
		makeSessionJSONL(t, claudeDir, dirName, row.id, lines)
	}

	// Budget = 8 → walker visits newest (chain-c) first and pulls 8 entries;
	// should NOT open chain-b or chain-a.
	entries := LoadHistoryChainTail(claudeDir, ids, cwd, 8)
	if len(entries) != 8 {
		t.Fatalf("expected 8 entries, got %d", len(entries))
	}
	// All entries should come from chain-c.
	for i, e := range entries {
		if !strings.HasPrefix(e.Summary, "chain-c-") {
			t.Errorf("entries[%d].Summary = %q, expected chain-c-*", i, e.Summary)
		}
	}
}

func TestLoadHistoryChainTail_SpillsIntoPriorSessions(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/chain-spill"
	dirName := projDirName(cwd)

	type entry struct{ id, tag string }
	rows := []entry{
		{"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "spill-a"},
		{"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "spill-b"},
		{"cccccccc-cccc-cccc-cccc-cccccccccccc", "spill-c"},
	}
	ids := make([]string, len(rows))
	for idx, row := range rows {
		ids[idx] = row.id
		lines := make([]string, 0, 5)
		for i := 0; i < 5; i++ {
			lines = append(lines, userJSONLLine("user", fmt.Sprintf("%s-%d", row.tag, i)))
		}
		makeSessionJSONL(t, claudeDir, dirName, row.id, lines)
	}

	// Budget = 12 → chain-c gives 5, chain-b gives 5, chain-a gives 2 → 12 total.
	entries := LoadHistoryChainTail(claudeDir, ids, cwd, 12)
	if len(entries) != 12 {
		t.Fatalf("expected 12 entries, got %d", len(entries))
	}

	// Chronological output should start inside chain-a and end at the end
	// of chain-c. Verify the first summary starts with "spill-a" and last
	// with "spill-c".
	first := entries[0].Summary
	last := entries[len(entries)-1].Summary
	if !strings.HasPrefix(first, "spill-a") {
		t.Errorf("first entry = %q, want spill-a-*", first)
	}
	if !strings.HasPrefix(last, "spill-c") {
		t.Errorf("last entry = %q, want spill-c-*", last)
	}
}

func TestLoadHistoryChainTail_EmptyInputs(t *testing.T) {
	claudeDir := makeClaudeDir(t)

	if got := LoadHistoryChainTail(claudeDir, nil, "/tmp/x", 10); got != nil {
		t.Errorf("expected nil for empty ids, got %d entries", len(got))
	}
	if got := LoadHistoryChainTail(claudeDir, []string{"a", "b"}, "/tmp/x", 0); got != nil {
		t.Errorf("expected nil for limit=0, got %d entries", len(got))
	}
	if got := LoadHistoryChainTail("", []string{"a"}, "/tmp/x", 10); got != nil {
		t.Errorf("expected nil for empty claudeDir, got %d entries", len(got))
	}
}

func TestLoadHistoryChainTail_SkipsMissing(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/chain-miss"
	dirName := projDirName(cwd)

	realID := "dddddddd-dddd-dddd-dddd-dddddddddddd"
	lines := []string{userJSONLLine("user", "real")}
	makeSessionJSONL(t, claudeDir, dirName, realID, lines)

	// Include non-UUID and missing-UUID IDs interleaved with the real one;
	// the walker must skip both categories and continue. Non-UUID entries
	// are rejected up front by R54-F5 defence.
	ids := []string{
		"missing-1",                            // non-UUID — skipped
		"eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee", // UUID but no file
		realID,
		"missing-2", // non-UUID — skipped
	}
	entries := LoadHistoryChainTail(claudeDir, ids, cwd, 10)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Summary != "real" {
		t.Errorf("summary = %q", entries[0].Summary)
	}
}

func TestLoadHistoryChainTail_RespectsCtxCancel(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/chain-ctx"
	dirName := projDirName(cwd)

	lines := []string{userJSONLLine("user", "present")}
	makeSessionJSONL(t, claudeDir, dirName, "ctx-id", lines)

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	// Sleep so the deadline definitely expires before the call starts.
	time.Sleep(5 * time.Millisecond)

	got := LoadHistoryChainTailCtx(ctx, claudeDir, []string{"ctx-id"}, cwd, 10)
	if got != nil {
		t.Errorf("expected nil on expired ctx, got %d entries", len(got))
	}
}

// ---------------------------------------------------------------------------
// resolveJSONLPath
// ---------------------------------------------------------------------------

func TestResolveJSONLPath_CWDHit(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/resolve"
	sessionID := "00000000-0000-0000-0000-000000001010"
	dirName := projDirName(cwd)

	_, jsonlPath := makeSessionJSONL(t, claudeDir, dirName, sessionID, []string{
		userJSONLLine("user", "x"),
	})

	got, err := resolveJSONLPath(claudeDir, sessionID, cwd)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != jsonlPath {
		t.Errorf("got %q, want %q", got, jsonlPath)
	}
}

func TestResolveJSONLPath_FallbackScan(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	sessionID := "00000000-0000-0000-0000-000000001011"
	_, jsonlPath := makeSessionJSONL(t, claudeDir, "-some-dir", sessionID, []string{
		userJSONLLine("user", "x"),
	})

	got, err := resolveJSONLPath(claudeDir, sessionID, "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != jsonlPath {
		t.Errorf("got %q, want %q", got, jsonlPath)
	}
}

func TestResolveJSONLPath_Missing(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	got, err := resolveJSONLPath(claudeDir, "no-such-id", "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty path, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Benchmark — tail vs legacy full read
// ---------------------------------------------------------------------------

func BenchmarkLoadHistoryTail_vs_LoadHistory(b *testing.B) {
	claudeDir := b.TempDir()
	cwd := "/tmp/bench"
	sessionID := "bench-session"
	dirName := projDirName(cwd)

	// 10,000 assistant lines; small but emulates realistic large sessions.
	lines := make([]string, 0, 10000)
	for i := 0; i < 10000; i++ {
		lines = append(lines, assistantJSONLLine(fmt.Sprintf("line %d reasonably long text content for realism", i)))
	}
	// Reuse the helper via a minimal path dance; writeJSONL isn't directly
	// accessible without test-only helpers but we can synthesize one here.
	_ = filepath.Join(claudeDir, "projects", dirName, sessionID+".jsonl")
	bt := &testing.T{}
	makeSessionJSONL(bt, claudeDir, dirName, sessionID, lines)

	b.Run("LoadHistory_full", func(b *testing.B) {
		for range b.N {
			_, _ = LoadHistory(claudeDir, sessionID, cwd)
		}
	})
	b.Run("LoadHistoryTail_500", func(b *testing.B) {
		for range b.N {
			_, _ = LoadHistoryTail(claudeDir, sessionID, cwd, 500)
		}
	})
}
