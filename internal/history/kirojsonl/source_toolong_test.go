package kirojsonl

import (
	"fmt"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// oversizedToolResults is a single ToolResults record well past the 1 MiB
// per-line scanner cap. Real kiro sessions here carry 20-40 MiB tool
// outputs on one line (#2448).
func oversizedToolResults(size int) string {
	return fmt.Sprintf(
		`{"version":"v1","kind":"ToolResults","data":{"message_id":"big","content":[{"kind":"toolResult","data":{"content":[{"kind":"text","data":"%s"}]}}]}}`,
		strings.Repeat("z", size))
}

func summaries(entries []clievent.EventEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Summary)
	}
	return out
}

// TestParseFile_SkipsOversizedLine pins #2448: a record longer than the
// scanner's 1 MiB cap used to surface as bufio.ErrTooLong, which ended the
// scan and silently dropped every record after it. The oversized record
// itself is not renderable and may be dropped, but everything that follows
// must still be parsed.
func TestParseFile_SkipsOversizedLine(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	sb.WriteString(promptLine("before", 1_700_000_000) + "\n")
	sb.WriteString(orphanAssistantLine("pre", 0))
	sb.WriteString(oversizedToolResults(3<<20) + "\n")
	for i := 0; i < 3; i++ {
		sb.WriteString(orphanAssistantLine("post", i))
	}
	sb.WriteString(promptLine("after", 1_700_000_100) + "\n")
	sb.WriteString(orphanAssistantLine("tail", 0))

	got := parseTempSession(t, sb.String())
	want := []string{"before", "pre-0", "post-0", "post-1", "post-2", "after", "tail-0"}
	if s := summaries(got); strings.Join(s, "|") != strings.Join(want, "|") {
		t.Fatalf("entries after oversized line lost\n got: %v\nwant: %v", s, want)
	}
	// The assistants after the oversized line still borrow the earlier
	// Prompt's ts with a continuous offset (the dropped record is not an
	// assistant, so it must not perturb the borrow state).
	for i, e := range got[1:5] {
		if want := int64(1_700_000_000*1000 + 1 + i); e.Time != want {
			t.Errorf("entry %q Time = %d, want %d", e.Summary, e.Time, want)
		}
	}
}

// TestParseFile_OversizedLineAtEOF: an unterminated oversized final record
// (writer still appending) must not lose the entries already collected.
func TestParseFile_OversizedLineAtEOF(t *testing.T) {
	t.Parallel()
	body := promptLine("p", 1_700_000_000) + "\n" +
		orphanAssistantLine("a", 0) +
		oversizedToolResults(2<<20) // no trailing newline
	got := parseTempSession(t, body)
	if s := summaries(got); strings.Join(s, "|") != "p|a-0" {
		t.Fatalf("got %v, want [p a-0]", s)
	}
}

// TestParseFile_TailWindowStartsInsideOversizedLine reproduces the live
// shape from #2448: the >maxFileBytes session's tail window begins in the
// middle of a multi-MiB ToolResults line. The straddling partial line is
// oversized too, so it must be skipped (no fragment reassembly) and the
// records that fill the rest of the window must all surface — previously
// the whole page came back empty.
func TestParseFile_TailWindowStartsInsideOversizedLine(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	sb.WriteString(promptLine("anchor", 1_700_000_000) + "\n")
	padWith(&sb, toolResultsFiller, 2<<20)
	sb.WriteString(oversizedToolResults(3<<20) + "\n")
	lineEnd := sb.Len()
	// ~14 MiB after the oversized line so off = Size-maxFileBytes lands
	// ~2 MiB before the line's end (i.e. inside it).
	const postAssistants = 5
	for i := 0; i < postAssistants; i++ {
		sb.WriteString(orphanAssistantLine("win", i))
		padWith(&sb, toolResultsFiller, sb.Len()+(14<<20)/postAssistants)
	}
	sb.WriteString(orphanAssistantLine("last", 0))
	off := sb.Len() - maxFileBytes
	if lineStart := lineEnd - len(oversizedToolResults(3<<20)) - 1; !(off > lineStart && off < lineEnd) {
		t.Fatalf("fixture: off=%d not inside oversized line [%d,%d)", off, lineStart, lineEnd)
	}

	got := parseTempSession(t, sb.String())
	if len(got) == 0 {
		t.Fatal("tail window returned no entries (scan aborted on oversized first line)")
	}
	want := make([]string, 0, postAssistants+1)
	for i := 0; i < postAssistants; i++ {
		want = append(want, fmt.Sprintf("win-%d", i))
	}
	want = append(want, "last-0")
	if s := summaries(got); strings.Join(s, "|") != strings.Join(want, "|") {
		t.Fatalf("window entries\n got: %v\nwant: %v", s, want)
	}
	// Borrowed from the head anchor Prompt (#2332) with global offsets.
	for i, e := range got {
		if want := int64(1_700_000_000*1000 + 1 + i); e.Time != want {
			t.Errorf("entry %q Time = %d, want %d", e.Summary, e.Time, want)
		}
	}
}

// TestParseFile_OversizedPartialFirstLineNotFedAsToken pins the F1 review
// finding on #2448: when the tail window's partial first line is oversized,
// the skip-branch Scan() fails with ErrTooLong, but a further Scan() on the
// same scanner hands back the buffered 1 MiB prefix as a final token (Go's
// split-at-EOF recovery). If that prefix happens to be a complete Prompt
// record it would be injected as a user entry and poison the borrow state.
// Layout: [anchor Prompt · 2 head assistants · filler] · oversized
// ToolResults line whose bytes from off onward begin with an exactly-1 MiB
// valid Prompt (decoyTS) followed by junk · ~13 MiB of window records.
func TestParseFile_OversizedPartialFirstLineNotFedAsToken(t *testing.T) {
	t.Parallel()
	const headAssistants = 2
	var head strings.Builder
	head.WriteString(promptLine("anchor", 1_700_000_000) + "\n")
	for i := 0; i < headAssistants; i++ {
		head.WriteString(orphanAssistantLine("head", i))
	}
	padWith(&head, toolResultsFiller, 1<<20)
	prefix := `{"version":"v1","kind":"ToolResults","data":{"message_id":"big","content":[{"kind":"toolResult","data":{"content":[{"kind":"text","data":"`
	base := len(promptLine("", decoyTS))
	injected := promptLine(strings.Repeat("q", maxLineBytes-base), decoyTS)
	if len(injected) != maxLineBytes {
		t.Fatalf("fixture: injected len=%d want %d", len(injected), maxLineBytes)
	}
	suffix := `"}}]}}` + "\n"

	var tail strings.Builder
	const winAssistants = 4
	for i := 0; i < winAssistants; i++ {
		tail.WriteString(orphanAssistantLine("win", i))
		padWith(&tail, toolResultsFiller, tail.Len()+(13<<20)/winAssistants)
	}
	tail.WriteString(orphanAssistantLine("last", 0))
	// off = Size-maxFileBytes must land exactly at the start of injected:
	// maxLineBytes + junk + len(suffix) + tail.Len() == maxFileBytes.
	junk := maxFileBytes - maxLineBytes - len(suffix) - tail.Len()
	if junk <= 0 {
		t.Fatalf("fixture: tail too long (%d)", tail.Len())
	}
	body := head.String() + prefix + injected + strings.Repeat("z", junk) + suffix + tail.String()
	if off := len(body) - maxFileBytes; off != head.Len()+len(prefix) {
		t.Fatalf("fixture: off=%d want %d", off, head.Len()+len(prefix))
	}

	got := parseTempSession(t, body)
	assertNoDecoyAnchor(t, got)
	for _, e := range got {
		if e.Type == "user" {
			t.Errorf("oversized-line prefix injected as user entry: %+v", e)
		}
	}
	want := make([]string, 0, winAssistants+1)
	for i := 0; i < winAssistants; i++ {
		want = append(want, fmt.Sprintf("win-%d", i))
	}
	want = append(want, "last-0")
	if s := summaries(got); strings.Join(s, "|") != strings.Join(want, "|") {
		t.Fatalf("window entries\n got: %v\nwant: %v", s, want)
	}
	for i, e := range got {
		if want := int64(1_700_000_000*1000 + 1 + headAssistants + i); e.Time != want {
			t.Errorf("entry %q Time = %d, want %d (borrow state poisoned?)", e.Summary, e.Time, want)
		}
	}
}
