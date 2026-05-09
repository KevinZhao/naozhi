package discovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// ---------------------------------------------------------------------------
// LoadHistoryTail
// ---------------------------------------------------------------------------

func TestLoadHistoryTail_LimitHonored(t *testing.T) {
	t.Parallel()
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

	entries, err := loadHistoryTail(claudeDir, sessionID, cwd, 10)
	if err != nil {
		t.Fatalf("loadHistoryTail error: %v", err)
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
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/tail-small"
	sessionID := "00000000-0000-0000-0000-000000001002"
	dirName := projDirName(cwd)

	lines := []string{
		userJSONLLine("user", "only-one"),
	}
	makeSessionJSONL(t, claudeDir, dirName, sessionID, lines)

	entries, err := loadHistoryTail(claudeDir, sessionID, cwd, 100)
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
	t.Parallel()
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

	entries, err := loadHistoryTail(claudeDir, sessionID, cwd, 10)
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
	t.Parallel()
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
	entries, err := loadHistoryTail(claudeDir, sessionID, cwd, 0)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (legacy path), got %d", len(entries))
	}
}

func TestLoadHistoryTail_MissingFile(t *testing.T) {
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	entries, err := loadHistoryTail(claudeDir, "does-not-exist", "", 50)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if entries != nil {
		t.Errorf("expected nil entries for missing file, got %v", entries)
	}
}

func TestLoadHistoryTail_SkipsMalformed(t *testing.T) {
	t.Parallel()
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

	entries, err := loadHistoryTail(claudeDir, sessionID, cwd, 10)
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

// TestParseTail_ScanBudgetBoundsIterations locks R54-SEC-006's byte budget.
// We call parseTail directly with a `size` 64× larger than the actual file,
// simulating a FUSE / /proc mount where stat().Size() lies. Without the
// scanBudget guard the loop would iterate `size / tailChunkSize` times —
// millions of ReadAt syscalls past EOF — each returning io.EOF silently
// but burning CPU walking 256KB zeroed buffers looking for '\n'.
// With the budget in place, the loop terminates within
// maxTailReadBytes / tailChunkSize = 512 iterations regardless of the
// claimed size.
//
// The test uses a ctx-deadline rather than wall-clock: ctx cancellation
// is checked at the top of every iteration, so the ctx itself provides
// an independent "did parseTail still honour its cancellation contract"
// signal in addition to time-bounded completion. We budget 15s under
// race for the ~512 iterations of (ReadAt→io.EOF→bytes.LastIndexByte over
// 256KB of zeros→carry book-keeping); on a clean tmpfs without -race
// this completes in <100ms. 15s is 100× that margin.
//
// We deliberately do NOT assert on returned entries: when size is lied
// about by 64×, parseTail reads from byte position 8GiB-256KB etc., which
// is past EOF — no entries are returned. This is the correct behaviour
// in a defence-in-depth context (we sacrifice usable results on a
// misbehaving mount to guarantee bounded work).
// Note: NOT t.Parallel() — this test swaps the package-level
// maxTailReadBytes var to shrink the budget, and any concurrent test
// that reads that var (LoadHistoryTailCtx, etc.) races with the write.
func TestParseTail_ScanBudgetBoundsIterations(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/tail-budget"
	sessionID := "00000000-0000-0000-0000-000000001099"
	dirName := projDirName(cwd)

	// Small real file so ReadAt past EOF yields io.EOF; only a handful of
	// lines actually present. parseTail handles io.EOF silently.
	lines := []string{
		userJSONLLine("user", "a"),
		userJSONLLine("user", "b"),
	}
	_, path := makeSessionJSONL(t, claudeDir, dirName, sessionID, lines)

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	// Dial the cap down to 2 MB for the test so the 512-iteration ceiling
	// becomes 8 iterations — fast enough under -race to leave ample slack
	// for the timeout check without dropping the real-world 128 MB default.
	// Restore on teardown. The regression is about ratio (budget ≪ claimed
	// size), not the absolute byte count; a 4 GB fake size against a 2 MB
	// cap proves the same bounded-work property as 8 GB against 128 MB.
	origCap := maxTailReadBytes
	maxTailReadBytes = 2 * 1024 * 1024
	t.Cleanup(func() { maxTailReadBytes = origCap })

	// Lie about the size: 2000× the cap. Without the scanBudget guard,
	// parseTail would loop `fakeSize / tailChunkSize` times. With the cap,
	// iterations are bounded to maxTailReadBytes / tailChunkSize = 8.
	fakeSize := 2000 * maxTailReadBytes

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := parseTail(ctx, f, fakeSize, 0, 10)
		done <- err
	}()

	select {
	case parseErr := <-done:
		if parseErr != nil && !errors.Is(parseErr, context.DeadlineExceeded) {
			t.Errorf("parseTail returned error: %v", parseErr)
		}
		if errors.Is(parseErr, context.DeadlineExceeded) {
			t.Fatal("parseTail exhausted ctx deadline — scanBudget cap broken")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("parseTail hung past ctx deadline — goroutine stuck")
	}
}

// TestParseTail_HonestSizeReturnsAllEntries complements the budget test: when
// stat.Size() is truthful and the file is well under maxTailReadBytes, the
// cap must not affect normal operation. Regression guard for "didn't
// accidentally change the hot path while adding the security cap".
func TestParseTail_HonestSizeReturnsAllEntries(t *testing.T) {
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/tail-honest"
	sessionID := "00000000-0000-0000-0000-000000001098"
	dirName := projDirName(cwd)

	lines := []string{
		userJSONLLine("user", "first"),
		userJSONLLine("user", "second"),
		userJSONLLine("user", "third"),
	}
	makeSessionJSONL(t, claudeDir, dirName, sessionID, lines)

	entries, err := loadHistoryTail(claudeDir, sessionID, cwd, 10)
	if err != nil {
		t.Fatalf("loadHistoryTail: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3 (honest size must not trigger cap)", len(entries))
	}
}

// Verify the cli.EventEntry import is still referenced after the test
// refactor; without this the build would fail silently if test code is
// reorganised and the declaration is no longer needed.
var _ = cli.EventEntry{}

func TestLoadHistoryTail_CancelledCtx(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	entries := loadHistoryChainTail(claudeDir, ids, cwd, 8)
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
	t.Parallel()
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
	entries := loadHistoryChainTail(claudeDir, ids, cwd, 12)
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
	t.Parallel()
	claudeDir := makeClaudeDir(t)

	if got := loadHistoryChainTail(claudeDir, nil, "/tmp/x", 10); got != nil {
		t.Errorf("expected nil for empty ids, got %d entries", len(got))
	}
	if got := loadHistoryChainTail(claudeDir, []string{"a", "b"}, "/tmp/x", 0); got != nil {
		t.Errorf("expected nil for limit=0, got %d entries", len(got))
	}
	if got := loadHistoryChainTail("", []string{"a"}, "/tmp/x", 10); got != nil {
		t.Errorf("expected nil for empty claudeDir, got %d entries", len(got))
	}
}

func TestLoadHistoryChainTail_SkipsMissing(t *testing.T) {
	t.Parallel()
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
	entries := loadHistoryChainTail(claudeDir, ids, cwd, 10)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Summary != "real" {
		t.Errorf("summary = %q", entries[0].Summary)
	}
}

func TestLoadHistoryChainTail_RespectsCtxCancel(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
// LoadHistoryTailBeforeCtx
// ---------------------------------------------------------------------------

// userJSONLLineAt returns a user-role JSONL line with the given unix second
// timestamp converted to RFC3339. Used by the "before" pagination tests so
// each record has a distinct, strictly-increasing Time.
func userJSONLLineAt(content string, unixSec int64) string {
	ts := time.Unix(unixSec, 0).UTC().Format(time.RFC3339)
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":%q}}`,
		ts, content)
}

func TestLoadHistoryTailBeforeCtx_FiltersByTime(t *testing.T) {
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/tail-before-basic"
	sessionID := "00000000-0000-0000-0000-0000000020a1"
	dirName := projDirName(cwd)

	// 10 user lines with strictly increasing timestamps 1000s..1009s.
	lines := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		lines = append(lines, userJSONLLineAt(fmt.Sprintf("msg-%d", i), int64(1000+i)))
	}
	makeSessionJSONL(t, claudeDir, dirName, sessionID, lines)

	// beforeMS = 1005s → only msg-0..msg-4 qualify; limit=10 so we get all 5
	// in chronological order.
	beforeMS := int64(1005) * 1000
	entries, err := LoadHistoryTailBeforeCtx(context.Background(), claudeDir, sessionID, cwd, beforeMS, 10)
	if err != nil {
		t.Fatalf("LoadHistoryTailBeforeCtx: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("expected 5 entries strictly older than %dms, got %d", beforeMS, len(entries))
	}
	if entries[0].Summary != "msg-0" {
		t.Errorf("entries[0].Summary = %q, want msg-0", entries[0].Summary)
	}
	if entries[4].Summary != "msg-4" {
		t.Errorf("entries[4].Summary = %q, want msg-4", entries[4].Summary)
	}
	for _, e := range entries {
		if e.Time >= beforeMS {
			t.Errorf("entry Time=%d not strictly < beforeMS=%d", e.Time, beforeMS)
		}
	}
}

func TestLoadHistoryTailBeforeCtx_StrictlyLess(t *testing.T) {
	// An entry whose Time equals beforeMS must be excluded. This pins the
	// "< (strict)" contract end-to-end so the dashboard, when passing the
	// oldest-rendered event's timestamp as beforeMS, never re-receives it.
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/tail-before-strict"
	sessionID := "00000000-0000-0000-0000-0000000020a2"
	dirName := projDirName(cwd)

	lines := []string{
		userJSONLLineAt("at-boundary", 1000),
		userJSONLLineAt("after", 1001),
	}
	makeSessionJSONL(t, claudeDir, dirName, sessionID, lines)

	beforeMS := int64(1000) * 1000
	entries, err := LoadHistoryTailBeforeCtx(context.Background(), claudeDir, sessionID, cwd, beforeMS, 10)
	if err != nil {
		t.Fatalf("LoadHistoryTailBeforeCtx: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries with Time == beforeMS must be excluded, got %d", len(entries))
	}
}

func TestLoadHistoryTailBeforeCtx_ZeroBeforeMatchesTail(t *testing.T) {
	// beforeMS=0 should degenerate to the plain LoadHistoryTailCtx so callers
	// that don't know a pagination cursor still get newest N.
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/tail-before-zero"
	sessionID := "00000000-0000-0000-0000-0000000020a3"
	dirName := projDirName(cwd)

	lines := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		lines = append(lines, userJSONLLineAt(fmt.Sprintf("m-%d", i), int64(2000+i)))
	}
	makeSessionJSONL(t, claudeDir, dirName, sessionID, lines)

	entries, err := LoadHistoryTailBeforeCtx(context.Background(), claudeDir, sessionID, cwd, 0, 3)
	if err != nil {
		t.Fatalf("LoadHistoryTailBeforeCtx: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 newest entries, got %d", len(entries))
	}
	if entries[0].Summary != "m-2" || entries[2].Summary != "m-4" {
		t.Errorf("got %q..%q, want m-2..m-4", entries[0].Summary, entries[2].Summary)
	}
}

func TestLoadHistoryTailBeforeCtx_LimitHonored(t *testing.T) {
	// Even when many entries qualify as "< beforeMS", only `limit` are
	// returned — the newest-of-qualifying tail.
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/tail-before-limit"
	sessionID := "00000000-0000-0000-0000-0000000020a4"
	dirName := projDirName(cwd)

	lines := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		lines = append(lines, userJSONLLineAt(fmt.Sprintf("n-%02d", i), int64(3000+i)))
	}
	makeSessionJSONL(t, claudeDir, dirName, sessionID, lines)

	// beforeMS past all entries; limit=5 should return the newest 5 (n-15..n-19).
	beforeMS := int64(9999) * 1000
	entries, err := LoadHistoryTailBeforeCtx(context.Background(), claudeDir, sessionID, cwd, beforeMS, 5)
	if err != nil {
		t.Fatalf("LoadHistoryTailBeforeCtx: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("expected 5 entries (limit), got %d", len(entries))
	}
	if entries[0].Summary != "n-15" || entries[4].Summary != "n-19" {
		t.Errorf("got %q..%q, want n-15..n-19", entries[0].Summary, entries[4].Summary)
	}
}

// ---------------------------------------------------------------------------
// LoadHistoryChainBeforeCtx
// ---------------------------------------------------------------------------

func TestLoadHistoryChainBeforeCtx_WalksOlderSessions(t *testing.T) {
	// Chain: two sessions, each 5 entries. beforeMS falls inside the newer
	// session → we get the older 2 of that session, then spill into the
	// older session for the remaining 3 to reach limit=5.
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/chain-before"
	dirName := projDirName(cwd)

	oldID := "11111111-1111-1111-1111-11111111aaa1"
	newID := "22222222-2222-2222-2222-22222222aaa2"
	// Older session: Time 1000s..1004s.
	oldLines := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		oldLines = append(oldLines, userJSONLLineAt(fmt.Sprintf("old-%d", i), int64(1000+i)))
	}
	makeSessionJSONL(t, claudeDir, dirName, oldID, oldLines)
	// Newer session: Time 2000s..2004s.
	newLines := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		newLines = append(newLines, userJSONLLineAt(fmt.Sprintf("new-%d", i), int64(2000+i)))
	}
	makeSessionJSONL(t, claudeDir, dirName, newID, newLines)

	// ids stored oldest→newest per caller contract.
	ids := []string{oldID, newID}

	// beforeMS = 2002s → qualifying entries are new-0, new-1 plus everything
	// in the old session (5). Limit=5 means we take new-1, new-0 (2 from
	// newer) then spill 3 from older: old-2, old-3, old-4. Returned in
	// chronological order.
	beforeMS := int64(2002) * 1000
	entries := LoadHistoryChainBeforeCtx(context.Background(), claudeDir, ids, cwd, beforeMS, 5)
	if len(entries) != 5 {
		t.Fatalf("expected 5 entries, got %d", len(entries))
	}
	// Strict < boundary check: no entry's Time may be >= beforeMS.
	for i, e := range entries {
		if e.Time >= beforeMS {
			t.Errorf("entries[%d] Time=%d >= beforeMS=%d", i, e.Time, beforeMS)
		}
	}
	// First entry should be from the older session.
	if !strings.HasPrefix(entries[0].Summary, "old-") {
		t.Errorf("entries[0] = %q, expected to spill into older session", entries[0].Summary)
	}
	// Last entry should be from the newer session (new-1 — the newest
	// qualifying entry).
	if entries[len(entries)-1].Summary != "new-1" {
		t.Errorf("entries[-1] = %q, expected new-1", entries[len(entries)-1].Summary)
	}
}

func TestLoadHistoryChainBeforeCtx_ZeroBeforeDelegates(t *testing.T) {
	// beforeMS <= 0 must behave identically to LoadHistoryChainTailCtx so
	// startup callers (which pass beforeMS=0 implicitly via the non-before
	// helpers) keep their legacy semantics.
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/chain-before-zero"
	dirName := projDirName(cwd)

	id := "33333333-3333-3333-3333-33333333aaa3"
	lines := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		lines = append(lines, userJSONLLineAt(fmt.Sprintf("z-%d", i), int64(4000+i)))
	}
	makeSessionJSONL(t, claudeDir, dirName, id, lines)

	got := LoadHistoryChainBeforeCtx(context.Background(), claudeDir, []string{id}, cwd, 0, 10)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries via tail delegation, got %d", len(got))
	}
}

func TestLoadHistoryChainBeforeCtx_EmptyInputs(t *testing.T) {
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	if got := LoadHistoryChainBeforeCtx(context.Background(), claudeDir, nil, "/tmp/x", 1234, 10); got != nil {
		t.Errorf("expected nil for empty ids, got %d", len(got))
	}
	if got := LoadHistoryChainBeforeCtx(context.Background(), claudeDir, []string{"a"}, "/tmp/x", 1234, 0); got != nil {
		t.Errorf("expected nil for limit=0, got %d", len(got))
	}
	if got := LoadHistoryChainBeforeCtx(context.Background(), "", []string{"a"}, "/tmp/x", 1234, 10); got != nil {
		t.Errorf("expected nil for empty claudeDir, got %d", len(got))
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
			_, _ = loadHistoryTail(claudeDir, sessionID, cwd, 500)
		}
	})
}
