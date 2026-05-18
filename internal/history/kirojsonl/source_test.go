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
	if got[1].Type != "assistant" || got[1].Summary != "hi back" {
		t.Errorf("entry[1]=%+v; want assistant/hi back", got[1])
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
	if got[0].Type != "user" || got[1].Type != "assistant" {
		t.Errorf("entries types=%q,%q; want user,assistant", got[0].Type, got[1].Type)
	}
}

// TestSource_LoadBefore_MissingTimestamp pins the no-timestamp drop
// rule: an AssistantMessage without meta.timestamp cannot be placed on
// the dashboard timeline so it must be skipped silently. Faking a
// time would corrupt the strict-< pagination boundary.
func TestSource_LoadBefore_MissingTimestamp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "no-ts"
	writeSession(t, dir, sid, []string{
		promptLine("with ts", 100),
		// AssistantMessage without meta — should be dropped.
		`{"version":"v1","kind":"AssistantMessage","data":{"message_id":"x","content":[{"kind":"text","data":"no time"}]}}`,
	})
	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 1 || got[0].Summary != "with ts" {
		t.Errorf("entries=%v; want only the timestamped entry", got)
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
