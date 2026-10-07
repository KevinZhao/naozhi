package subagent

import (
	"os"
	"testing"
)

const spanBase = 1778407200000 // 2026-05-10T10:00:00Z in unix ms

func spanText(text, ts string) string {
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + text + `"}]},"sessionId":"s"`
	if ts != "" {
		line += `,"timestamp":"` + ts + `"`
	}
	return line + "}\n"
}

func wantSpan(t *testing.T, r *TranscriptReader, first, last int64) {
	t.Helper()
	if f, l := r.Span(); f != first || l != last {
		t.Fatalf("Span() = %d, %d; want %d, %d", f, l, first, last)
	}
}

// Span runs from the earliest record to the latest whatever order they were
// written in, and leaves out records without a timestamp (#3646).
func TestTranscriptReader_SpanEarliestToLatest(t *testing.T) {
	t.Parallel()
	path := tmpFile(t, spanText("b", "2026-05-10T10:00:20Z")+spanText("c", "2026-05-10T10:00:45.9Z")+
		spanText("unstamped", "")+spanText("a", "2026-05-10T10:00:01Z"))
	r := NewTranscriptReader(path)
	defer r.Close() //nolint:errcheck
	wantSpan(t, r, 0, 0)
	if _, err := r.Tail(); err != nil {
		t.Fatalf("Tail: %v", err)
	}
	wantSpan(t, r, spanBase+1000, spanBase+45900)
}

// A teammate's transcript opens with its prompt in a teammate-message
// wrapper, which maps to no event; its timestamp still starts the run, or
// the first model turn would be left out of the agent's duration.
func TestTranscriptReader_SpanCountsDroppedRecords(t *testing.T) {
	t.Parallel()
	prompt := `{"type":"user","message":{"role":"user","content":"<teammate-message teammate_id=\"lister-1\">do it</teammate-message>"},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}` + "\n"
	bookkeeping := `{"type":"file-history-snapshot","timestamp":"2026-05-09T00:00:00Z"}` + "\n"
	path := tmpFile(t, bookkeeping+prompt+spanText("reply", "2026-05-10T10:00:12Z"))
	r := NewTranscriptReader(path)
	defer r.Close() //nolint:errcheck
	ents, err := r.Tail()
	if err != nil || len(ents) != 1 {
		t.Fatalf("Tail = %+v, %v; want the reply only", ents, err)
	}
	wantSpan(t, r, spanBase, spanBase+12000)
}

// A transcript replaced by another file starts its own span: the old file's
// bounds must not stretch the new one's.
func TestTranscriptReader_SpanResetsOnRotation(t *testing.T) {
	t.Parallel()
	path := tmpFile(t, spanText("old", "2026-05-10T09:00:00Z")+spanText("old2", "2026-05-10T09:30:00Z"))
	r := NewTranscriptReader(path)
	defer r.Close() //nolint:errcheck
	if _, err := r.Tail(); err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.WriteFile(path, []byte(spanText("new", "2026-05-10T10:00:00Z")+spanText("new2", "2026-05-10T10:00:03Z")), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if ents, err := r.Tail(); err != nil || len(ents) != 2 {
		t.Fatalf("post-rotation Tail = %+v, %v", ents, err)
	}
	wantSpan(t, r, spanBase, spanBase+3000)
}
