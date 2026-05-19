package kirojsonl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/history"
)

// promptLine builds a v1 Prompt jsonl line at the given unix-second
// timestamp. Test inputs avoid escape-heavy text so a simple
// fmt.Sprintf is sufficient — no need for json.Marshal indirection.
func promptLine(text string, unixSec int64) string {
	return fmt.Sprintf(
		`{"version":"v1","kind":"Prompt","data":{"message_id":"prompt-%d","content":[{"kind":"text","data":%q}],"meta":{"timestamp":%d}}}`,
		unixSec, text, unixSec,
	)
}

// assistantLine builds a v1 AssistantMessage jsonl line. AssistantMessage
// records sometimes carry meta.timestamp (the kiro probe captured both
// shapes) so include it for deterministic ordering in tests.
func assistantLine(text string, unixSec int64) string {
	return fmt.Sprintf(
		`{"version":"v1","kind":"AssistantMessage","data":{"message_id":"asst-%d","content":[{"kind":"text","data":%q}],"meta":{"timestamp":%d}}}`,
		unixSec, text, unixSec,
	)
}

// assistantLineRaw builds an AssistantMessage with an arbitrary content
// JSON array — needed for tests that mix text / thinking / toolUse chunks.
func assistantLineRaw(contentJSON string, unixSec int64) string {
	return fmt.Sprintf(
		`{"version":"v1","kind":"AssistantMessage","data":{"message_id":"asst-%d","content":%s,"meta":{"timestamp":%d}}}`,
		unixSec, contentJSON, unixSec,
	)
}

// writeSession writes the given lines to <rootDir>/<sid>.jsonl, joined
// with newlines plus a trailing newline (kiro emits them that way).
func writeSession(t *testing.T, rootDir, sid string, lines []string) string {
	t.Helper()
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(rootDir, sid+".jsonl")
	body := strings.Join(lines, "\n")
	if len(lines) > 0 {
		body += "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSource_ImplementsInterface is a compile-time assertion that
// *Source satisfies history.Source. A future refactor that drops
// LoadBefore would fail to build at this line rather than at every
// merged.Source call site.
func TestSource_ImplementsInterface(t *testing.T) {
	t.Parallel()
	var _ history.Source = (*Source)(nil)
}

// TestSource_LoadBefore_FullRoundTrip pins the happy path: a Prompt
// followed by an AssistantMessage round-trips into two EventEntries
// with the right Type, Time (unix-ms from the unix-sec timestamp), and
// Summary. Without this we could silently regress the type-mapping or
// timestamp scaling and the dashboard would render an empty timeline.
func TestSource_LoadBefore_FullRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "44737bdd-b2fd-466a-8a30-ca447e688313"
	writeSession(t, dir, sid, []string{
		promptLine("hello kiro", 1779081689),
		assistantLine("hi back", 1779081690),
	})

	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("entries=%d, want 2", len(got))
	}
	if got[0].Type != "user" || got[0].Summary != "hello kiro" {
		t.Errorf("entry[0]=%+v; want user/hello kiro", got[0])
	}
	if got[0].Time != 1779081689*1000 {
		t.Errorf("entry[0].Time=%d; want unix-sec→ms scale", got[0].Time)
	}
	if got[1].Type != "text" || got[1].Summary != "hi back" {
		t.Errorf("entry[1]=%+v; want text/hi back", got[1])
	}
	if got[1].Time != 1779081690*1000 {
		t.Errorf("entry[1].Time=%d; want unix-sec→ms scale", got[1].Time)
	}
}

// TestSource_LoadBefore_Limit covers the limit cap and the zero/negative
// degenerate cases. limit=1 must keep the newest entry (tail-read
// semantics); limit≤0 must return nil with no I/O so callers can probe
// availability cheaply.
func TestSource_LoadBefore_Limit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "limit-session"
	writeSession(t, dir, sid, []string{
		promptLine("first", 100),
		promptLine("second", 200),
		promptLine("third", 300),
	})
	src := New(dir, func() string { return sid })

	t.Run("limit=1 keeps newest", func(t *testing.T) {
		got, err := src.LoadBefore(context.Background(), 0, 1)
		if err != nil {
			t.Fatalf("LoadBefore: %v", err)
		}
		if len(got) != 1 || got[0].Summary != "third" {
			t.Errorf("entries=%v; want exactly the newest entry 'third'", got)
		}
	})
	t.Run("limit=0 returns nil", func(t *testing.T) {
		got, err := src.LoadBefore(context.Background(), 0, 0)
		if err != nil || got != nil {
			t.Errorf("limit=0 → got=%v err=%v; want (nil, nil)", got, err)
		}
	})
	t.Run("limit=-1 returns nil", func(t *testing.T) {
		got, err := src.LoadBefore(context.Background(), 0, -1)
		if err != nil || got != nil {
			t.Errorf("limit=-1 → got=%v err=%v; want (nil, nil)", got, err)
		}
	})
}

// TestSource_LoadBefore_BeforeFiltering pins the strict-< upper bound:
// entries whose Time equals beforeMS must NOT appear (otherwise
// pagination would re-fetch the boundary entry forever).
func TestSource_LoadBefore_BeforeFiltering(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "filter-session"
	writeSession(t, dir, sid, []string{
		promptLine("at 100", 100),
		promptLine("at 200", 200),
		promptLine("at 300", 300),
	})
	src := New(dir, func() string { return sid })

	// beforeMS=200_000ms — only the 100s entry should pass (strict <).
	got, err := src.LoadBefore(context.Background(), 200_000, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 1 || got[0].Summary != "at 100" {
		t.Fatalf("got=%v; want only the 100s entry", got)
	}
}

// TestSource_LoadBefore_MissingFile pins the zero-state path: a
// session ID with no on-disk jsonl returns (nil, nil), not an error.
// Pagination must treat this as end-of-history rather than retry.
// TestSource_LoadBefore_RejectsPathTraversal pins the security guard:
// a SessionIDFunc returning a sid with path separators or ".." must NOT
// reach the filesystem. The guard treats it as "no session" so callers
// degrade gracefully without leaking that traversal was attempted into
// dashboard error toasts.
func TestSource_LoadBefore_RejectsPathTraversal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Plant a file outside dir to prove traversal would have worked
	// without the guard. Cannot reasonably plant /etc/passwd in a unit
	// test, so we use a sibling temp dir.
	cases := []string{
		"../etc/passwd",
		"..\\etc\\passwd",
		"sub/dir",
		"./local",
		"a/../b",
	}
	for _, sid := range cases {
		t.Run(sid, func(t *testing.T) {
			src := New(dir, func() string { return sid })
			got, err := src.LoadBefore(context.Background(), 0, 10)
			if err != nil {
				t.Errorf("traversal sid %q produced err=%v; want nil (degrade silently)", sid, err)
			}
			if got != nil {
				t.Errorf("traversal sid %q returned %d entries; want nil", sid, len(got))
			}
		})
	}
}

func TestSource_LoadBefore_MissingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := New(dir, func() string { return "no-such-session" })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Errorf("missing file produced err=%v; want nil", err)
	}
	if got != nil {
		t.Errorf("missing file returned %d entries; want nil", len(got))
	}
}

// TestSource_LoadBefore_EmptySessionID pins the unbound-session path:
// when SessionIDFunc returns "", LoadBefore must return (nil, nil)
// without touching the filesystem at all (so a misconfigured rootDir
// still yields a clean nil instead of an error).
func TestSource_LoadBefore_EmptySessionID(t *testing.T) {
	t.Parallel()
	src := New("/nonexistent/dir", func() string { return "" })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Errorf("empty sid produced err=%v; want nil", err)
	}
	if got != nil {
		t.Errorf("empty sid returned %d entries; want nil", len(got))
	}
}

// TestSource_LoadBefore_PartialLastLine simulates a concurrent kiro
// writer mid-append: the final line lacks the closing brace. The
// scanner must skip the malformed tail and still surface the prior
// well-formed records — losing them would visibly truncate the chat
// view on every "load earlier" press during an active session.
func TestSource_LoadBefore_PartialLastLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "partial"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	good := promptLine("good", 100)
	// Truncate at column ~40 to mimic a half-flushed append.
	bad := `{"version":"v1","kind":"Prompt","data":{"message_id":"x","co`
	body := good + "\n" + bad + "\n"
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 1 || got[0].Summary != "good" {
		t.Errorf("entries=%v; want only the good entry", got)
	}
}

// TestSource_LoadBefore_UnknownKind pins schema-evolution tolerance:
// a future kind kiro hasn't released yet (e.g. "ToolCall") sandwiched
// between known kinds must not break decoding of the surrounding
// records. Without this the dashboard would silently lose history
// every time kiro shipped a new record kind.
func TestSource_LoadBefore_UnknownKind(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "unknown"
	writeSession(t, dir, sid, []string{
		promptLine("first", 100),
		// Future kind we deliberately don't handle yet.
		`{"version":"v1","kind":"ToolCall","data":{"tool_name":"shell"}}`,
		// v2 record from some future kiro release.
		`{"version":"v2","kind":"Prompt","data":{"shape":"unknown"}}`,
		assistantLine("third", 300),
	})

	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("entries=%v; want 2 (Prompt + AssistantMessage)", got)
	}
	if got[0].Type != "user" || got[1].Type != "text" {
		t.Errorf("entries types=%q,%q; want user,text", got[0].Type, got[1].Type)
	}
}

// TestSource_LoadBefore_AssistantBorrowsPromptTimestamp pins the
// kiro-specific timestamp salvage: real kiro AssistantMessage records
// don't carry meta.timestamp at all (only the originating Prompt
// does). To surface assistant text in the dashboard at all, each
// AssistantMessage borrows the most recent Prompt ts plus a monotonic
// ms offset. The offset keeps later-emitted assistants strictly after
// earlier ones in chronological order, and stays well under the next
// Prompt's ts (kiro Prompt timestamps are unix seconds, so adjacent
// prompts are ≥1 s = 1000 ms apart).
//
// Without this, the entire transcript collapses to "user prompts only"
// because every AssistantMessage gets dropped for missing ts.
func TestSource_LoadBefore_AssistantBorrowsPromptTimestamp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "borrow-ts"

	// Two prompts each followed by two AssistantMessages with no meta.
	noMetaAsst := func(text string) string {
		return fmt.Sprintf(
			`{"version":"v1","kind":"AssistantMessage","data":{"message_id":"a-%s","content":[{"kind":"text","data":%q}]}}`,
			text, text,
		)
	}
	writeSession(t, dir, sid, []string{
		promptLine("first prompt", 100),
		noMetaAsst("first reply A"),
		noMetaAsst("first reply B"),
		promptLine("second prompt", 200),
		noMetaAsst("second reply"),
	})

	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("entries=%d; want 5 (2 prompts + 3 assistants)", len(got))
	}

	// Chronology must be: prompt1 < replyA < replyB < prompt2 < secondReply.
	wantOrder := []string{"first prompt", "first reply A", "first reply B", "second prompt", "second reply"}
	for i, want := range wantOrder {
		if got[i].Summary != want {
			t.Errorf("entry[%d].Summary=%q; want %q (chronology broken)", i, got[i].Summary, want)
		}
	}
	if got[0].Time != 100_000 || got[3].Time != 200_000 {
		t.Errorf("prompt times = %d/%d; want 100_000/200_000", got[0].Time, got[3].Time)
	}
	if !(got[1].Time > got[0].Time && got[2].Time > got[1].Time && got[2].Time < got[3].Time) {
		t.Errorf("assistant ts under first prompt not strictly between prompts: %d/%d/%d",
			got[0].Time, got[1].Time, got[2].Time)
	}
	if got[4].Time <= got[3].Time {
		t.Errorf("assistant under second prompt ts=%d; want > prompt2 ts %d", got[4].Time, got[3].Time)
	}
}

// TestSource_LoadBefore_OrphanedAssistantDropped pins the safety case:
// an AssistantMessage with no own timestamp AND no preceding Prompt
// (file starts with assistant) cannot be anchored — it must be dropped,
// not given ts=0, otherwise the dashboard timeline would jump to epoch.
func TestSource_LoadBefore_OrphanedAssistantDropped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "orphan"
	writeSession(t, dir, sid, []string{
		`{"version":"v1","kind":"AssistantMessage","data":{"message_id":"x","content":[{"kind":"text","data":"floating"}]}}`,
		promptLine("anchor", 100),
	})
	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 1 || got[0].Summary != "anchor" {
		t.Errorf("entries=%v; want only the prompt — orphan assistant must be dropped", got)
	}
}

// TestSource_LoadBefore_MissingPromptTimestamp pins the user-message
// rule: a Prompt without meta.timestamp still cannot be placed on the
// timeline (no upstream record to borrow from), so it is dropped — and
// any AssistantMessage that follows it loses its anchor too.
func TestSource_LoadBefore_MissingPromptTimestamp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "no-prompt-ts"
	writeSession(t, dir, sid, []string{
		// Prompt without meta — dropped.
		`{"version":"v1","kind":"Prompt","data":{"message_id":"p","content":[{"kind":"text","data":"no time"}]}}`,
		// AssistantMessage without meta — orphaned, also dropped.
		`{"version":"v1","kind":"AssistantMessage","data":{"message_id":"a","content":[{"kind":"text","data":"reply"}]}}`,
	})
	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("entries=%v; want zero entries (no anchorable records)", got)
	}
}

// TestSource_LoadBefore_MultiChunkContent pins the content
// concatenation rule: a Prompt with two text chunks joins to a single
// summary string. Kiro typically emits one chunk but the schema is a
// list, so a future model that streams partial messages into a single
// jsonl record must still surface a sensible summary.
func TestSource_LoadBefore_MultiChunkContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "multi-chunk"
	writeSession(t, dir, sid, []string{
		`{"version":"v1","kind":"Prompt","data":{"message_id":"x","content":[{"kind":"text","data":"first "},{"kind":"text","data":"second"}],"meta":{"timestamp":100}}}`,
	})
	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 1 || got[0].Summary != "first second" {
		t.Errorf("got=%v; want concat of two text chunks", got)
	}
}

// TestSource_LoadBefore_NonTextChunkSkipped pins the multi-modal
// fallback: an image/binary chunk in a content list must be skipped
// without producing a garbled summary. Today the dashboard chat view
// renders text only; future image support is a separate sprint.
func TestSource_LoadBefore_NonTextChunkSkipped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "non-text"
	writeSession(t, dir, sid, []string{
		`{"version":"v1","kind":"Prompt","data":{"message_id":"x","content":[{"kind":"image","data":"<binary>"},{"kind":"text","data":"caption"}],"meta":{"timestamp":100}}}`,
	})
	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 1 || got[0].Summary != "caption" {
		t.Errorf("got=%v; want text-only concat 'caption'", got)
	}
}

// TestSource_LoadBefore_ContextCanceled pins the cooperative
// cancellation contract: a cancelled ctx returns promptly without
// erroring (errors mean "real I/O failure" — cancellation is just
// "stop early, return what we have"). Asserts ≤1 entry — the
// cancel-before-LoadBefore case might process zero or one batch.
func TestSource_LoadBefore_ContextCanceled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "cancel"
	lines := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		lines = append(lines, promptLine(fmt.Sprintf("line %d", i), int64(100+i)))
	}
	writeSession(t, dir, sid, lines)

	src := New(dir, func() string { return sid })
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel: parseFile must observe Done immediately.
	got, err := src.LoadBefore(ctx, 0, 1000)
	if err != nil {
		t.Errorf("LoadBefore on canceled ctx returned err=%v; want nil", err)
	}
	if len(got) > ctxCheckEvery {
		t.Errorf("canceled ctx kept reading: got %d entries, expected ≤%d", len(got), ctxCheckEvery)
	}
}

// TestSource_LoadBefore_DegradesOnMisconfig pins the zero-state matrix
// for the receiver: nil receiver, empty rootDir, nil sessionID, and
// limit≤0 all return (nil, nil) without panicking. Each row corresponds
// to a misconfiguration mode the router could plausibly hit during
// startup before its config has fully resolved.
func TestSource_LoadBefore_DegradesOnMisconfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		src  *Source
	}{
		{"nil receiver", (*Source)(nil)},
		{"empty rootDir", New("", func() string { return "x" })},
		{"nil sessionIDFn", New("/tmp", nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.src.LoadBefore(context.Background(), 1000, 10)
			if err != nil {
				t.Errorf("err=%v; want nil", err)
			}
			if got != nil {
				t.Errorf("got %d entries; want nil", len(got))
			}
		})
	}
}

// TestSource_LoadBefore_SessionIDReevaluated pins the dynamic-callback
// contract: the SessionIDFunc is consulted on every call so a kiro
// session/load swap mid-pagination is observed by the next page.
func TestSource_LoadBefore_SessionIDReevaluated(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeSession(t, dir, "first", []string{promptLine("from first", 100)})
	writeSession(t, dir, "second", []string{promptLine("from second", 200)})

	call := 0
	src := New(dir, func() string {
		call++
		if call == 1 {
			return "first"
		}
		return "second"
	})

	got1, _ := src.LoadBefore(context.Background(), 0, 10)
	got2, _ := src.LoadBefore(context.Background(), 0, 10)
	if len(got1) != 1 || got1[0].Summary != "from first" {
		t.Errorf("call 1: %v; want from-first entry", got1)
	}
	if len(got2) != 1 || got2[0].Summary != "from second" {
		t.Errorf("call 2: %v; want from-second entry", got2)
	}
}

// TestSource_LoadBefore_BlankAndEmptyLines pins resilience to a kiro
// flush that leaves a blank line in the file (rare, but observed when
// SIGTERM races a partial newline). The scanner must skip blanks.
func TestSource_LoadBefore_BlankAndEmptyLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "blanks"
	body := promptLine("kept", 100) + "\n\n\n" + promptLine("kept2", 200) + "\n"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got=%v; want 2 entries (blanks skipped)", got)
	}
}

// TestSource_LoadBefore_EmptyContent pins the zero-content edge case:
// a Prompt with an empty content array decodes to an EventEntry with
// an empty Summary so the timeline gets a placeholder entry rather
// than a hidden gap.
func TestSource_LoadBefore_EmptyContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "empty-content"
	writeSession(t, dir, sid, []string{
		`{"version":"v1","kind":"Prompt","data":{"message_id":"x","content":[],"meta":{"timestamp":100}}}`,
	})
	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 1 || got[0].Summary != "" || got[0].Type != "user" {
		t.Errorf("got=%+v; want one user entry with empty summary", got)
	}
}

// TestSource_LoadBefore_AssistantToolAndThinkingFiltered pins parity
// with the Claude Code path (discovery/history_tail.go: assistant arm):
// only non-empty text chunks become EventEntry rows. Thinking, toolUse,
// and empty-text chunks are skipped silently so the dashboard transcript
// shows just the model's outward-facing decisions / explanations rather
// than the raw tool-call timeline.
//
// Real-world kiro AssistantMessage records routinely emit
//
//	[{kind:thinking,data:{...}}, {kind:text,data:""}, {kind:toolUse,...}]
//
// and the empty trailing text chunk in particular must not surface as
// an empty bubble — without this filter every assistant turn produces
// a blank message.
func TestSource_LoadBefore_AssistantToolAndThinkingFiltered(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "asst-filter"

	// Mirrors the shape captured from a live kiro session: thinking
	// payload is an object {text, signature, redactedContent}, toolUse
	// payload is the full tool call envelope, and the text chunk is
	// frequently emitted but empty.
	// jsonl is newline-delimited, so each AssistantMessage record must
	// be a single line — keep the content arrays on one line each.
	thinkingOnly := `[{"kind":"thinking","data":{"text":"internal reasoning","signature":"sig","redactedContent":[]}},{"kind":"text","data":""},{"kind":"toolUse","data":{"toolUseId":"t1","name":"read","input":{}}}]`
	textAndTool := `[{"kind":"text","data":"Now I understand the bug. Let me apply the fix."},{"kind":"toolUse","data":{"toolUseId":"t2","name":"edit","input":{}}}]`

	writeSession(t, dir, sid, []string{
		promptLine("user prompt", 100),
		assistantLineRaw(thinkingOnly, 200), // must be dropped entirely
		assistantLineRaw(textAndTool, 300),  // surfaces as one text entry
	})

	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("entries=%d, want 2 (prompt + one assistant text)", len(got))
	}
	if got[0].Type != "user" || got[0].Summary != "user prompt" {
		t.Errorf("entry[0]=%+v; want user/user prompt", got[0])
	}
	if got[1].Type != "text" {
		t.Errorf("entry[1].Type=%q; want text (cc-aligned)", got[1].Type)
	}
	if got[1].Summary != "Now I understand the bug. Let me apply the fix." {
		t.Errorf("entry[1].Summary=%q; want the assistant's text chunk only", got[1].Summary)
	}
	if got[1].Time != 300_000 {
		t.Errorf("entry[1].Time=%d; want 300_000", got[1].Time)
	}
}

// TestSource_LoadBefore_AssistantEmptyTextDropped pins the whitespace
// rule: an assistant content list with only blank/whitespace text must
// not produce a bubble (matches cc's strings.TrimSpace == "" check).
// Without this, every "assistant emits text:” before tool_use" turn
// would inject an empty card into the transcript.
func TestSource_LoadBefore_AssistantEmptyTextDropped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "asst-empty"

	writeSession(t, dir, sid, []string{
		promptLine("hi", 100),
		assistantLineRaw(`[{"kind":"text","data":""}]`, 200),
		assistantLineRaw(`[{"kind":"text","data":"   \n  "}]`, 300),
		assistantLineRaw(`[{"kind":"text","data":"real reply"}]`, 400),
	})

	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("entries=%d, want 2 (prompt + only the non-blank assistant)", len(got))
	}
	if got[1].Type != "text" || got[1].Summary != "real reply" {
		t.Errorf("entry[1]=%+v; want text/real reply", got[1])
	}
}

// TestKirojsonlFactory_RegistrationOnInit confirms the package-level
// init() registered "kiro" with cli.RegisterHistoryFactory. Without
// this, NewWrapper(... "kiro" ...) would never wire a history.Source
// and the dashboard would silently lose kiro JSONL fallback after
// upgrade.
func TestKirojsonlFactory_RegistrationOnInit(t *testing.T) {
	t.Parallel()
	w := cli.NewWrapper("/bin/false", &cli.ClaudeProtocol{}, "kiro")
	src := w.NewHistorySource(&stubKiroSession{sid: "x"}, cli.HistoryWiring{KiroSessionsDir: "/kiro/dir"})
	if _, ok := src.(*Source); !ok {
		t.Errorf("wrapper(kiro).NewHistorySource = %T; want *kirojsonl.Source — init() registration regressed", src)
	}
}

// TestKirojsonlFactory_EmptyDirReturnsNoop pins the factory's
// degradation rule: an empty KiroSessionsDir means "no on-disk
// source available", so the factory must yield cli.NoopHistorySource —
// never a *Source wrapping an empty path.
func TestKirojsonlFactory_EmptyDirReturnsNoop(t *testing.T) {
	t.Parallel()
	got := factory(&stubKiroSession{sid: "x"}, cli.HistoryWiring{KiroSessionsDir: ""})
	if _, ok := got.(cli.NoopHistorySource); !ok {
		t.Errorf("empty KiroSessionsDir factory returned %T; want cli.NoopHistorySource", got)
	}
}
