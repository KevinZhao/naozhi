package kirojsonl

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// Fixture pieces shared by the #2332 tests.

// toolResultsFiller is a 4 KiB ToolResults record: it carries neither the
// Prompt nor the AssistantMessage marker, and its length guarantees that
// lines straddle the 1 MiB reverse-scan chunk boundaries.
var toolResultsFiller = fmt.Sprintf(
	`{"version":"v1","kind":"ToolResults","data":{"message_id":"tr","content":[{"kind":"toolResult","data":{"content":[{"kind":"text","data":%q}]}}]}}`+"\n",
	strings.Repeat("x", 4096),
)

// decoyTS is the alien timestamp carried by the decoys below; no entry may
// ever be anchored on it.
const decoyTS int64 = 1_799_999_999

// promptMarkerDecoy is a ToolResults record whose content holds a
// structured {"kind":"Prompt"} chunk plus a meta.timestamp, so the byte
// quick-filter matches the Prompt marker and only kind-aware decoding
// rejects it as an anchor.
var promptMarkerDecoy = fmt.Sprintf(
	`{"version":"v1","kind":"ToolResults","data":{"message_id":"decoy","content":[{"kind":"Prompt","data":"x"}],"meta":{"timestamp":%d}}}`+"\n", decoyTS)

// orphanAssistantLine is an AssistantMessage with no meta.timestamp whose
// text is "<prefix>-<i>".
func orphanAssistantLine(prefix string, i int) string {
	return fmt.Sprintf(
		`{"version":"v1","kind":"AssistantMessage","data":{"message_id":"a-%s-%d","content":[{"kind":"text","data":"%s-%d"}]}}`+"\n",
		prefix, i, prefix, i)
}

// promptChunkAssistantLine is an AssistantMessage (no meta.timestamp) whose
// content embeds a {"kind":"Prompt"} chunk before its text. Both byte
// markers match; it must still be counted as a borrowed-ts assistant.
func promptChunkAssistantLine(prefix string, i int) string {
	return fmt.Sprintf(
		`{"version":"v1","kind":"AssistantMessage","data":{"message_id":"a-%s-%d","content":[{"kind":"Prompt","data":"x"},{"kind":"text","data":"%s-%d"}]}}`+"\n",
		prefix, i, prefix, i)
}

func padWith(sb *strings.Builder, line string, until int) {
	for sb.Len() < until {
		sb.WriteString(line)
	}
}

func parseTempSession(t *testing.T, body string) []clievent.EventEntry {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}
	return parseSessionFile(t, dir, path)
}

func parseSessionFile(t *testing.T, dir, path string) []clievent.EventEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	s := New(dir, func() string { return "s" })
	return s.parseFile(context.Background(), f, 0)
}

func assertNoDecoyAnchor(t *testing.T, got []clievent.EventEntry) {
	t.Helper()
	for _, e := range got {
		if e.Time >= decoyTS*1000 && e.Time < (decoyTS+1)*1000 {
			t.Errorf("entry anchored on the decoy timestamp: %+v", e)
		}
	}
}

// TestParseFile_TailSeekAnchorsAssistantsFromDroppedHead pins #2332: in a
// >maxFileBytes kiro session the tail window can begin long after the most
// recent Prompt (real sessions here have a single Prompt followed by ~100 MiB
// of ToolResults/AssistantMessage). AssistantMessage records carry no
// meta.timestamp and borrow the last Prompt's ts; when that Prompt lives in
// the discarded head, every in-window assistant was dropped and LoadBefore
// returned nothing at all. parseFile must scan the head backwards for the
// anchoring Prompt and seed the borrow state from it.
//
// Layout (offsets approximate): [0] ANCHOR Prompt · 5 plain head assistants
// + 1 assistant embedding a {"kind":"Prompt"} chunk · [~0.5 MiB] decoy ·
// filler to 17 MiB · 3 in-window orphan assistants. The decoy sits inside
// the discarded head so the backscan meets it (and must reject it) before
// reaching the real Prompt.
func TestParseFile_TailSeekAnchorsAssistantsFromDroppedHead(t *testing.T) {
	t.Parallel()
	const anchorTS int64 = 1_700_000_000
	const headAssistants = 6

	var sb strings.Builder
	sb.WriteString(promptLine("ANCHOR prompt in dropped head", anchorTS) + "\n")
	for i := 1; i < headAssistants; i++ {
		sb.WriteString(orphanAssistantLine("HEAD", i))
	}
	sb.WriteString(promptChunkAssistantLine("HEAD", headAssistants))
	padWith(&sb, toolResultsFiller, 512<<10)
	decoyOff := sb.Len()
	sb.WriteString(promptMarkerDecoy)
	padWith(&sb, toolResultsFiller, 17<<20)
	for i := 1; i <= 3; i++ {
		sb.WriteString(orphanAssistantLine("ORPHAN", i))
	}
	if windowStart := sb.Len() - maxFileBytes; decoyOff >= windowStart {
		t.Fatalf("fixture: decoy at %d is inside the tail window starting at %d", decoyOff, windowStart)
	}

	got := parseTempSession(t, sb.String())
	assertNoDecoyAnchor(t, got)

	var orphans []string
	for _, e := range got {
		if strings.Contains(e.Summary, "ANCHOR") || strings.HasPrefix(e.Summary, "HEAD-") {
			t.Errorf("head record outside the window leaked in as an entry: %+v", e)
		}
		if strings.HasPrefix(e.Summary, "ORPHAN-") {
			orphans = append(orphans, e.Summary)
			// Global sequence: the 6 head assistants come first, so the
			// in-window ones are anchor+1+6, +7, +8 — identical to a
			// whole-file parse.
			want := anchorTS*1000 + headAssistants + int64(len(orphans))
			if e.Type != "text" || e.Time != want {
				t.Errorf("%s: type=%q time=%d; want text anchored at %d", e.Summary, e.Type, e.Time, want)
			}
		}
	}
	if len(orphans) != 3 {
		t.Fatalf("#2332: in-window assistants anchored to a head Prompt were dropped; got %v, want 3", orphans)
	}
}

// TestParseFile_TailSeekAnchorSkipsTimestamplessPrompt: a head Prompt without
// meta.timestamp cannot anchor anything (decodeLine drops it and the forward
// scan does not reset the borrow state on it), so the backscan must keep
// walking to the earlier ts-bearing Prompt and keep counting assistants
// across the ts-less one.
func TestParseFile_TailSeekAnchorSkipsTimestamplessPrompt(t *testing.T) {
	t.Parallel()
	const anchorTS int64 = 1_700_000_000

	var sb strings.Builder
	sb.WriteString(promptLine("ANCHOR", anchorTS) + "\n")
	sb.WriteString(orphanAssistantLine("HEAD", 1))
	padWith(&sb, toolResultsFiller, 256<<10)
	sb.WriteString(`{"version":"v1","kind":"Prompt","data":{"message_id":"nots","content":[{"kind":"text","data":"no timestamp"}]}}` + "\n")
	sb.WriteString(orphanAssistantLine("HEAD", 2))
	padWith(&sb, toolResultsFiller, 17<<20)
	sb.WriteString(orphanAssistantLine("ORPHAN", 1))

	got := parseTempSession(t, sb.String())
	var found bool
	for _, e := range got {
		if e.Summary == "ORPHAN-1" {
			found = true
			if want := anchorTS*1000 + 2 + 1; e.Time != want {
				t.Errorf("ORPHAN-1 time=%d; want %d (anchor + 2 head assistants + 1)", e.Time, want)
			}
		}
	}
	if !found {
		t.Fatalf("ORPHAN-1 dropped; ts-less head Prompt must be skipped, not treated as end of search: %+v", got)
	}
}

// TestParseFile_TailSeekAnchorStableAcrossAppend pins the review finding on
// #2446: the borrowed ts must not depend on where the seek point falls. As
// kiro appends, off = Size-maxFileBytes moves right; if in-window assistants
// were numbered from 0 at the window start, the same record would get a
// smaller ts and a different UUID on the next LoadBefore, and the dashboard's
// load-older pagination (before = oldest visible ts) would render it twice.
//
// It also asserts the record straddling the seek point is recovered from the
// backscan fragment rather than dropped (its presence or absence would
// otherwise shift every later sequence number by one as off slides).
//
// Layout: ANCHOR Prompt · ToolResults filler to ~0.9 MiB · 600 assistants
// (4 KiB each, spanning ~0.9–3.3 MiB) · ToolResults filler to 17 MiB · 3
// tail assistants. The seek point (~1 MiB) lands inside the assistant block,
// so the window opens on a cut assistant with ~25 more already in the head.
func TestParseFile_TailSeekAnchorStableAcrossAppend(t *testing.T) {
	t.Parallel()
	const anchorTS int64 = 1_700_000_000
	pad := strings.Repeat("y", 4096)
	asstLine := func(n int) string {
		return fmt.Sprintf(
			`{"version":"v1","kind":"AssistantMessage","data":{"message_id":"a-%d","content":[{"kind":"text","data":"ASST-%d %s"}]}}`+"\n", n, n, pad)
	}

	var sb strings.Builder
	sb.WriteString(promptLine("ANCHOR", anchorTS) + "\n")
	padWith(&sb, toolResultsFiller, 900<<10)
	n := 0
	for ; n < 600; n++ {
		sb.WriteString(asstLine(n + 1))
	}
	padWith(&sb, toolResultsFiller, 17<<20)
	for i := 0; i < 3; i++ {
		n++
		sb.WriteString(asstLine(n))
	}
	body := sb.String()

	// Locate the record cut by the seek point and its global index.
	off := len(body) - maxFileBytes
	lineStart := bytes.LastIndexByte([]byte(body[:off]), '\n') + 1
	lineEnd := lineStart + bytes.IndexByte([]byte(body[lineStart:]), '\n')
	var straddleIdx int64
	if _, err := fmt.Sscanf(body[lineStart:lineEnd], `{"version":"v1","kind":"AssistantMessage","data":{"message_id":"a-%d"`, &straddleIdx); err != nil || lineStart >= off {
		t.Fatalf("fixture: seek point %d must cut an assistant record (line %d..%d): %v", off, lineStart, lineEnd, err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}

	snapshot := func() map[string]clievent.EventEntry {
		m := make(map[string]clievent.EventEntry)
		for _, e := range parseSessionFile(t, dir, path) {
			m[strings.Fields(e.Summary)[0]] = e
		}
		return m
	}
	before := snapshot()
	if len(before) == 0 {
		t.Fatal("no entries surfaced from oversized session")
	}
	// Every surfaced assistant must carry its whole-file sequence number.
	for key, e := range before {
		var idx int64
		if _, err := fmt.Sscanf(key, "ASST-%d", &idx); err != nil {
			t.Fatalf("unexpected summary %q", key)
		}
		if want := anchorTS*1000 + idx; e.Time != want {
			t.Errorf("%s time=%d; want %d (global index, independent of window start)", key, e.Time, want)
		}
	}
	straddleKey := fmt.Sprintf("ASST-%d", straddleIdx)
	if e, ok := before[straddleKey]; !ok {
		t.Errorf("record straddling the seek point (%s) was dropped; want it reassembled from the head fragment", straddleKey)
	} else if want := anchorTS*1000 + straddleIdx; e.Time != want {
		t.Errorf("%s (straddling) time=%d; want %d", straddleKey, e.Time, want)
	}

	// Append a few more turns so off moves past the first in-window records.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	for i := 0; i < 3; i++ {
		n++
		fmt.Fprint(f, asstLine(n))
	}
	f.Close()
	after := snapshot()

	compared := 0
	for key, b := range before {
		a, ok := after[key]
		if !ok {
			continue // fell out of the window as it slid right — expected
		}
		compared++
		if a.Time != b.Time || a.UUID != b.UUID {
			t.Errorf("%s drifted after append: time %d→%d uuid %s→%s", key, b.Time, a.Time, b.UUID, a.UUID)
		}
	}
	if compared < 100 {
		t.Fatalf("only %d records overlapped between snapshots; fixture too small", compared)
	}
}
