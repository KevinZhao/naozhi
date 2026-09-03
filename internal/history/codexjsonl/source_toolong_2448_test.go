package codexjsonl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// oversizedOutputLine is a single non-renderable rollout record well past
// the 1 MiB per-line scanner cap (a large function_call_output).
func oversizedOutputLine(size int) string {
	return `{"timestamp":"2026-06-21T09:10:00.000Z","type":"response_item","payload":{"type":"function_call_output","output":"` +
		strings.Repeat("z", size) + `"}}`
}

func msgLine(kind, text, ts string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":%q,"message":%q}}`, ts, kind, text)
}

// TestSource_LoadBefore_SkipsOversizedLine pins #2448 for codex rollouts: a
// record longer than the scanner cap used to end the scan with
// bufio.ErrTooLong, dropping every record after it.
func TestSource_LoadBefore_SkipsOversizedLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "019ee988-da7f-7821-b6d1-7b74a7db62d7"
	writeRollout(t, dir, sid, []string{
		msgLine("user_message", "before", "2026-06-21T09:00:00.000Z"),
		oversizedOutputLine(3 << 20),
		msgLine("agent_message", "after-1", "2026-06-21T09:00:01.000Z"),
		msgLine("user_message", "after-2", "2026-06-21T09:00:02.000Z"),
	})
	got, err := New(dir, func() string { return sid }).LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	var s []string
	for _, e := range got {
		s = append(s, e.Summary)
	}
	if strings.Join(s, "|") != "before|after-1|after-2" {
		t.Fatalf("entries after oversized line lost: %v", s)
	}
}

// TestSource_LoadBefore_TailWindowStartsInsideOversizedLine: the tail window
// of a >maxFileBytes rollout begins inside an oversized line; the partial
// first line is skipped and the rest of the window must still surface.
func TestSource_LoadBefore_TailWindowStartsInsideOversizedLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "019ee988-da7f-7821-b6d1-7b74a7db62d8"
	var sb strings.Builder
	sb.WriteString(msgLine("user_message", "OLDEST", "2026-06-21T08:00:00.000Z") + "\n")
	filler := `{"timestamp":"2026-06-21T09:10:00.000Z","type":"response_item","payload":{"type":"function_call_output","output":"` + strings.Repeat("x", 4096) + `"}}` + "\n"
	for sb.Len() < 2<<20 {
		sb.WriteString(filler)
	}
	big := oversizedOutputLine(3 << 20)
	sb.WriteString(big + "\n")
	lineEnd := sb.Len()
	const n = 5
	for i := 0; i < n; i++ {
		sb.WriteString(msgLine("agent_message", fmt.Sprintf("win-%d", i), fmt.Sprintf("2026-06-21T09:30:0%d.000Z", i)) + "\n")
		for target := sb.Len() + (14<<20)/n; sb.Len() < target; {
			sb.WriteString(filler)
		}
	}
	sb.WriteString(msgLine("agent_message", "NEWEST", "2026-06-21T09:59:59.000Z") + "\n")
	off := sb.Len() - maxFileBytes
	if lineStart := lineEnd - len(big) - 1; !(off > lineStart && off < lineEnd) {
		t.Fatalf("fixture: off=%d not inside oversized line [%d,%d)", off, lineStart, lineEnd)
	}
	bucket := filepath.Join(dir, "2026", "06", "21")
	if err := os.MkdirAll(bucket, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bucket, "rollout-2026-06-21T09-00-00-"+sid+".jsonl"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := New(dir, func() string { return sid }).LoadBefore(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("tail window returned no entries (scan aborted on oversized first line)")
	}
	var s []string
	for _, e := range got {
		s = append(s, e.Summary)
	}
	want := "win-0|win-1|win-2|win-3|win-4|NEWEST"
	if strings.Join(s, "|") != want {
		t.Fatalf("window entries\n got: %v\nwant: %s", s, want)
	}
}

// TestSource_LoadBefore_OversizedPartialFirstLineNotFedAsToken pins the F1
// review finding on #2448: after the skip-branch Scan() fails with
// ErrTooLong, a further Scan() on the same scanner returns the buffered
// 1 MiB prefix as a final token; if it is a complete user_message record it
// is injected into the page. Here off lands exactly where an exactly-1 MiB
// valid record begins inside an oversized line.
func TestSource_LoadBefore_OversizedPartialFirstLineNotFedAsToken(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "019ee988-da7f-7821-b6d1-7b74a7db62d9"
	filler := `{"timestamp":"2026-06-21T09:10:00.000Z","type":"response_item","payload":{"type":"function_call_output","output":"` + strings.Repeat("x", 4096) + `"}}` + "\n"
	var head strings.Builder
	head.WriteString(msgLine("user_message", "OLDEST", "2026-06-21T08:00:00.000Z") + "\n")
	for head.Len() < 1<<20 {
		head.WriteString(filler)
	}
	prefix := `{"timestamp":"2026-06-21T09:10:00.000Z","type":"response_item","payload":{"type":"function_call_output","output":"`
	base := len(msgLine("user_message", "", "2026-06-21T09:20:00.000Z"))
	injected := msgLine("user_message", strings.Repeat("q", maxLineBytes-base), "2026-06-21T09:20:00.000Z")
	if len(injected) != maxLineBytes {
		t.Fatalf("fixture: injected len=%d want %d", len(injected), maxLineBytes)
	}
	suffix := `"}}` + "\n"
	var tail strings.Builder
	const n = 4
	for i := 0; i < n; i++ {
		tail.WriteString(msgLine("agent_message", fmt.Sprintf("win-%d", i), fmt.Sprintf("2026-06-21T09:30:0%d.000Z", i)) + "\n")
		for target := tail.Len() + (13<<20)/n; tail.Len() < target; {
			tail.WriteString(filler)
		}
	}
	tail.WriteString(msgLine("agent_message", "NEWEST", "2026-06-21T09:59:59.000Z") + "\n")
	junk := maxFileBytes - maxLineBytes - len(suffix) - tail.Len()
	if junk <= 0 {
		t.Fatalf("fixture: tail too long (%d)", tail.Len())
	}
	body := head.String() + prefix + injected + strings.Repeat("z", junk) + suffix + tail.String()
	if off := len(body) - maxFileBytes; off != head.Len()+len(prefix) {
		t.Fatalf("fixture: off=%d want %d", off, head.Len()+len(prefix))
	}
	bucket := filepath.Join(dir, "2026", "06", "21")
	if err := os.MkdirAll(bucket, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bucket, "rollout-2026-06-21T09-00-00-"+sid+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := New(dir, func() string { return sid }).LoadBefore(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	var s []string
	for _, e := range got {
		s = append(s, e.Summary)
	}
	want := "win-0|win-1|win-2|win-3|NEWEST"
	if strings.Join(s, "|") != want {
		t.Fatalf("oversized-line prefix injected or window lost\n got: %v\nwant: %s", s, want)
	}
}
