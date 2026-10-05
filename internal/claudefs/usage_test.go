package claudefs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

const usageSID = "2420ea6d-c992-4327-90f7-a0c7a992867e"

var usageT0 = time.Date(2026, 10, 3, 14, 0, 0, 0, time.UTC)

// usageLine is one assistant line, at usageT0 plus sec seconds.
func usageLine(sec int, id, model string, in, out, cr, cw int64) string {
	b, _ := json.Marshal(map[string]any{
		"type": "assistant", "timestamp": usageT0.Add(time.Duration(sec) * time.Second).Format(time.RFC3339Nano),
		"sessionId": usageSID,
		"message": map[string]any{"id": id, "model": model, "role": "assistant",
			"content": []any{map[string]any{"type": "text", "text": "x"}},
			"usage": map[string]any{"input_tokens": in, "output_tokens": out,
				"cache_read_input_tokens": cr, "cache_creation_input_tokens": cw}},
	})
	return string(b)
}

func writeLines(t *testing.T, path string, mtime time.Time, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// The window sums the main loop, a sub-agent and a hosted-workflow agent:
// lines at or before Since and after Until are out, the per-block lines of one
// message count once at their largest values, and a stale sub-agent file and
// a workflow journal are never read.
func TestSessionUsage_SumsMainSubagentAndWorkflowInWindow(t *testing.T) {
	proj := t.TempDir()
	sub := SubagentsDir(proj, usageSID)
	recent := usageT0.Add(time.Hour)
	writeLines(t, TranscriptIn(proj, usageSID), recent,
		usageLine(5, "msg_old", "claude-opus-5-5", 1, 1, 1, 1), // at Since: out
		usageLine(6, "msg_a", "claude-opus-5-5", 3, 8, 100, 21584),
		usageLine(7, "msg_a", "claude-opus-5-5", 3, 224, 100, 21584), // same message, later block
		`{"type":"user","message":{"content":"not usage"}}`,
		usageLine(9, "msg_b", "claude-opus-5-5", 2, 10, 0, 0),
		usageLine(31, "msg_late", "claude-opus-5-5", 9, 9, 9, 9), // after Until: out
	)
	writeLines(t, SubagentJSONL(sub, "a1"), recent,
		usageLine(8, "msg_s", "claude-sonnet-5", 4, 40, 0, 400))
	writeLines(t, filepath.Join(sub, "workflows", "wf_1", "agent-a2.jsonl"), recent,
		usageLine(10, "msg_w", "anthropic.claude-haiku-4-5-20251001-v1:0", 10, 98, 0, 15784),
		usageLine(10, "msg_w", "anthropic.claude-haiku-4-5-20251001-v1:0", 10, 98, 0, 15784))
	writeLines(t, filepath.Join(sub, "workflows", "wf_1", "journal.jsonl"), recent,
		usageLine(10, "msg_j", "claude-opus-5-5", 1000, 1000, 0, 0))
	writeLines(t, SubagentJSONL(sub, "stale"), usageT0, // last written before the window
		usageLine(12, "msg_stale", "claude-opus-5-5", 1000, 1000, 0, 0))

	got, found, err := SessionUsage(proj, usageSID, UsageWindow{Since: usageT0.Add(5 * time.Second), Until: usageT0.Add(30 * time.Second)})
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	want := []ModelTokens{
		{Model: "claude-opus-5-5", Input: 5, Output: 234, CacheRead: 100, CacheWrite: 21584},
		{Model: "claude-sonnet-5", Input: 4, Output: 40, CacheWrite: 400},
		{Model: "anthropic.claude-haiku-4-5-20251001-v1:0", Input: 10, Output: 98, CacheWrite: 15784},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("usage = %+v\nwant    %+v", got, want)
	}
}

// MainOffset skips what the main transcript held before the window's process
// wrote to it, even lines whose timestamps fall in the window; an offset past
// the end (the file was replaced) reads it all.
func TestSessionUsage_MainOffsetSkipsEarlierBytes(t *testing.T) {
	proj := t.TempDir()
	before := usageLine(6, "msg_before", "m", 50, 50, 0, 0) + "\n"
	path := TranscriptIn(proj, usageSID)
	writeLines(t, path, usageT0, strings.TrimSuffix(before, "\n"), usageLine(7, "msg_after", "m", 1, 2, 0, 0))
	w := UsageWindow{Since: usageT0, MainOffset: int64(len(before))}
	got, _, err := SessionUsage(proj, usageSID, w)
	if err != nil || !reflect.DeepEqual(got, []ModelTokens{{Model: "m", Input: 1, Output: 2}}) {
		t.Fatalf("from offset: usage = %+v err=%v", got, err)
	}
	w.MainOffset = 1 << 30
	got, _, _ = SessionUsage(proj, usageSID, w)
	if !reflect.DeepEqual(got, []ModelTokens{{Model: "m", Input: 51, Output: 52}}) {
		t.Fatalf("offset past the end: usage = %+v, want the whole file", got)
	}
}

// No main transcript is "not found", not an error, and so is a session id
// that is not one (it would become a path).
func TestSessionUsage_MissingTranscriptNotFound(t *testing.T) {
	proj := t.TempDir()
	for _, sid := range []string{usageSID, "../../etc/passwd", ""} {
		if got, found, err := SessionUsage(proj, sid, UsageWindow{}); found || err != nil || got != nil {
			t.Fatalf("sid %q: usage=%v found=%v err=%v", sid, got, found, err)
		}
	}
}

// A symlinked agent transcript or workflow directory is not followed, though
// what it points at holds in-window usage.
func TestSessionUsage_SymlinksNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	proj := t.TempDir()
	sub := SubagentsDir(proj, usageSID)
	recent := usageT0.Add(time.Hour)
	writeLines(t, TranscriptIn(proj, usageSID), recent, usageLine(6, "msg_main", "m", 1, 2, 0, 0))
	outside := t.TempDir()
	target := filepath.Join(outside, "agent-real.jsonl")
	writeLines(t, target, recent, usageLine(7, "msg_link", "m", 1000, 1000, 0, 0))
	if err := os.MkdirAll(filepath.Join(sub, "workflows"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, SubagentJSONL(sub, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(sub, "workflows", "wf_link")); err != nil {
		t.Fatal(err)
	}

	got, _, err := SessionUsage(proj, usageSID, UsageWindow{Since: usageT0, Until: usageT0.Add(time.Minute)})
	if err != nil || !reflect.DeepEqual(got, []ModelTokens{{Model: "m", Input: 1, Output: 2}}) {
		t.Fatalf("usage = %+v err=%v, want the main transcript's line only", got, err)
	}
}

// The whole history comes message by message, each timed and tagged by its
// first line, across the main and workflow transcripts, and DayTotals splits
// it per UTC day; a message another session already counted (a fork's copy
// of its parent's lines) is skipped, and this session's ids join the counted
// set.
func TestSessionMessageUsage_TagsEachMessageAndSkipsCountedOnes(t *testing.T) {
	proj := t.TempDir()
	sub := SubagentsDir(proj, usageSID)
	day := 24 * 60 * 60
	writeLines(t, TranscriptIn(proj, usageSID), usageT0,
		usageLine(0, "msg_parent", "m", 500, 500, 0, 0), // copied from the parent
		withEntrypoint(usageLine(1, "msg_a", "m", 1, 2, 3, 4), "cli"),
		withEntrypoint(usageLine(day, "msg_a", "m", 1, 9, 3, 4), "sdk-cli"), // a later block of the same message: day one
		usageLine(day+1, "msg_b", "m", 10, 0, 0, 0),
		usageLine(day+3, "msg_empty", "m", 0, 0, 0, 0))
	writeLines(t, filepath.Join(sub, "workflows", "wf_1", "agent-w.jsonl"), usageT0,
		withEntrypoint(usageLine(day+2, "msg_w", "n", 0, 7, 0, 0), "sdk-cli"))

	counted := map[string]bool{"msg_parent": true}
	got, found, err := SessionMessageUsage(proj, usageSID, counted)
	if err != nil || !found || got.Truncated {
		t.Fatalf("found=%v truncated=%v err=%v", found, got.Truncated, err)
	}
	at := func(sec int) time.Time { return usageT0.Add(time.Duration(sec) * time.Second) }
	want := []MessageUsage{
		{ModelTokens: ModelTokens{Model: "m", Input: 1, Output: 9, CacheRead: 3, CacheWrite: 4}, At: at(1), Entrypoint: "cli"},
		{ModelTokens: ModelTokens{Model: "m", Input: 10}, At: at(day + 1)},
		{ModelTokens: ModelTokens{Model: "n", Output: 7}, At: at(day + 2), Entrypoint: "sdk-cli"},
	}
	if !reflect.DeepEqual(got.Messages, want) {
		t.Fatalf("messages = %+v\nwant       %+v", got.Messages, want)
	}
	days := map[string][]ModelTokens{
		"2026-10-03": {{Model: "m", Input: 1, Output: 9, CacheRead: 3, CacheWrite: 4}},
		"2026-10-04": {{Model: "m", Input: 10}, {Model: "n", Output: 7}},
	}
	if got := DayTotals(got.Messages); !reflect.DeepEqual(got, days) {
		t.Fatalf("days = %+v\nwant   %+v", got, days)
	}
	for _, id := range []string{"msg_a", "msg_b", "msg_w"} {
		if !counted[id] {
			t.Errorf("%s not added to the counted set", id)
		}
	}
	again, _, _ := SessionMessageUsage(proj, usageSID, counted)
	if len(again.Messages) != 0 {
		t.Errorf("a second session holding the same messages counted %+v, want nothing", again.Messages)
	}
}

// An assistant line without a timestamp counts in no window, the whole
// history's included.
func TestSessionMessageUsage_UntimedLineCountsNowhere(t *testing.T) {
	proj := t.TempDir()
	untimed := strings.Replace(usageLine(1, "msg_untimed", "m", 1000, 0, 0, 0), `"timestamp"`, `"stamp"`, 1)
	writeLines(t, TranscriptIn(proj, usageSID), usageT0, untimed, usageLine(2, "msg_timed", "m", 1, 0, 0, 0))
	got, _, err := SessionMessageUsage(proj, usageSID, map[string]bool{})
	if err != nil || len(got.Messages) != 1 || got.Messages[0].Input != 1 {
		t.Fatalf("messages = %+v err=%v, want the timed line only", got.Messages, err)
	}
}

func withEntrypoint(line, entrypoint string) string {
	var v map[string]any
	_ = json.Unmarshal([]byte(line), &v)
	v["entrypoint"] = entrypoint
	b, _ := json.Marshal(v)
	return string(b)
}

// Past maxUsageFiles agent transcripts the rest go unread, and the result
// says so: it is a lower bound.
func TestSessionMessageUsage_ReportsTruncation(t *testing.T) {
	proj := t.TempDir()
	sub := SubagentsDir(proj, usageSID)
	writeLines(t, TranscriptIn(proj, usageSID), usageT0, usageLine(1, "msg_main", "m", 1, 0, 0, 0))
	for i := range maxUsageFiles + 1 {
		writeLines(t, SubagentJSONL(sub, fmt.Sprintf("a%d", i)), usageT0, usageLine(2, fmt.Sprintf("msg_%d", i), "m", 1, 0, 0, 0))
	}
	got, _, err := SessionMessageUsage(proj, usageSID, map[string]bool{})
	if err != nil || !got.Truncated {
		t.Fatalf("truncated=%v err=%v, want truncated", got.Truncated, err)
	}
	if n := DayTotals(got.Messages)["2026-10-03"][0].Input; n != maxUsageFiles+1 {
		t.Errorf("input = %d, want the main line plus %d agent files", n, maxUsageFiles)
	}
}

// padded is line with a pad field of n bytes.
func padded(line string, n int) string {
	var v map[string]any
	_ = json.Unmarshal([]byte(line), &v)
	v["pad"] = strings.Repeat("x", n)
	b, _ := json.Marshal(v)
	return string(b)
}

// userLine is a user line at usageT0 plus sec seconds, n bytes of padding.
func userLine(sec, n int) string {
	return padded(fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"q"}}`,
		usageT0.Add(time.Duration(sec)*time.Second).Format(time.RFC3339Nano)), n)
}

// linearUsage is what reading every line of path through w's filter gives.
func linearUsage(t *testing.T, path string, w UsageWindow) []ModelTokens {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	acc := newUsageAcc(w)
	if err := eachLine(f, maxUsageLine, acc.line); err != nil {
		t.Fatal(err)
	}
	return acc.totals()
}

func writeMainTranscript(t *testing.T, proj, body string) string {
	t.Helper()
	path := TranscriptIn(proj, usageSID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A window near the end of a large transcript starts reading close to it,
// and sums what a line-by-line read does. The file holds what real
// transcripts do: timestamp-less lines, a snapshot whose nested timestamp is
// in the window, an overlong line, a stretch without usage lines longer than
// a probe reads, a message whose blocks straddle Since, and user lines
// stamped hours before the assistant lines around them. The read starts
// before every usage line within seekSlack of Since.
func TestSessionUsage_LargeTranscriptSeeksNearItsWindow(t *testing.T) {
	const since = 10000
	in := usageT0.Add(since * time.Second).Format(time.RFC3339Nano)
	var b strings.Builder
	add := func(lines ...string) {
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
	}
	for i := range 2485 {
		add(padded(usageLine(i*4, fmt.Sprintf("msg_old%d", i/2), "m", 1000, 0, 0, 0), 4<<10))
		switch {
		case i%50 == 0:
			add(`{"type":"summary","summary":"s"}`,
				fmt.Sprintf(`{"type":"file-history-snapshot","snapshot":{"timestamp":%q}}`, in))
		case i == 301:
			add(padded(usageLine(i*4, "msg_long", "m", 1000, 0, 0, 0), 2<<20))
		case i == 601:
			for range 640 {
				add(userLine(i*4, 8<<10))
			}
		}
	}
	late := int64(b.Len()) // the usage lines within seekSlack of Since, more than a margin of them
	for sec := since - 59; sec < since-1; sec++ {
		add(padded(usageLine(sec, fmt.Sprintf("msg_late%d", sec), "m", 1000, 0, 0, 0), 64<<10))
	}
	add(usageLine(since-1, "msg_straddle", "m", 7, 0, 0, 0))
	for i := range 1200 {
		for range 4 {
			add(userLine(100, 2<<10))
		}
		if i == 0 {
			add(usageLine(since+1, "msg_straddle", "m", 7, 0, 0, 0))
		}
		add(usageLine(since+1+i, fmt.Sprintf("msg_in%d", i), "m", 1, 0, 0, 0))
	}
	proj := t.TempDir()
	path := writeMainTranscript(t, proj, b.String())
	w := UsageWindow{Since: usageT0.Add(since * time.Second), Until: usageT0.Add((since + 1001) * time.Second)}

	got, found, err := SessionUsage(proj, usageSID, w)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	want := []ModelTokens{{Model: "m", Input: 1001 + 7}}
	if lin := linearUsage(t, path, w); !reflect.DeepEqual(lin, want) || !reflect.DeepEqual(got, want) {
		t.Fatalf("usage = %+v, line by line %+v, want %+v", got, lin, want)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	start := usageStart(f, 0, w.Since)
	if start > late || start < late-2*maxUsageLine {
		t.Fatalf("read starts at %d of %d, want within %d bytes before %d", start, b.Len(), 2*maxUsageLine, late)
	}
	if replaced := usageStart(f, 1<<40, w.Since); replaced != start {
		t.Fatalf("an offset past the end starts at %d, want %d as from the start", replaced, start)
	}
}

// A probe finding no usage line within its reach moves the start no further:
// the usage lines before a long stretch of user lines (tool results) in the
// window still count.
func TestSessionUsage_ProbeMissKeepsTheStart(t *testing.T) {
	var b strings.Builder
	for i := range 500 {
		b.WriteString(padded(usageLine(i, fmt.Sprintf("msg_old%d", i), "m", 1000, 0, 0, 0), 4<<10) + "\n")
	}
	b.WriteString(usageLine(1000, "msg_before_stretch", "m", 1, 0, 0, 0) + "\n")
	for b.Len() < 16<<20 {
		b.WriteString(userLine(1001, 8<<10) + "\n")
	}
	b.WriteString(usageLine(1002, "msg_after_stretch", "m", 2, 0, 0, 0) + "\n")
	proj := t.TempDir()
	writeMainTranscript(t, proj, b.String())
	got, _, err := SessionUsage(proj, usageSID, UsageWindow{Since: usageT0.Add(999 * time.Second)})
	if err != nil || !reflect.DeepEqual(got, []ModelTokens{{Model: "m", Input: 3}}) {
		t.Fatalf("usage = %+v err=%v, want both lines in the window", got, err)
	}
}

// With no more than seekPast bytes to read, every line is read: a usage line
// out of time order still counts.
func TestSessionUsage_ShortReadTakesEveryLine(t *testing.T) {
	var b strings.Builder
	b.WriteString(usageLine(5000, "msg_early_in_window", "m", 1, 0, 0, 0) + "\n")
	const size = 6 << 20 // more than a bisect step, less than seekPast
	for i := 0; b.Len() < size; i++ {
		b.WriteString(padded(usageLine(i, fmt.Sprintf("msg_old%d", i), "m", 1000, 0, 0, 0), 4<<10) + "\n")
	}
	if b.Len() > seekPast {
		t.Fatalf("the transcript holds %d bytes, more than seekPast", b.Len())
	}
	proj := t.TempDir()
	writeMainTranscript(t, proj, b.String())
	got, _, err := SessionUsage(proj, usageSID, UsageWindow{Since: usageT0.Add(4500 * time.Second)})
	if err != nil || !reflect.DeepEqual(got, []ModelTokens{{Model: "m", Input: 1}}) {
		t.Fatalf("usage = %+v err=%v, want the early line", got, err)
	}
}

// eachLineAt's offsets count every byte before a line, overlong lines it
// skips and lines longer than its buffer included, and fn can stop it.
func TestEachLineAt_OffsetsCountSkippedLines(t *testing.T) {
	lines := []string{"a\n", strings.Repeat("b", 100<<10) + "\n", strings.Repeat("c", 300) + "\n", "d\n", "e\n", "f"}
	type seen struct {
		off int64
		n   int
	}
	var got []seen
	err := eachLineAt(strings.NewReader(strings.Join(lines, "")), 100<<10+1, func(off int64, line []byte) bool {
		got = append(got, seen{off, len(line)})
		return line[0] != 'e'
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []seen{{0, 2}, {2, 100<<10 + 1}, {2 + 100<<10 + 1, 301}, {2 + 100<<10 + 302, 2}, {2 + 100<<10 + 304, 2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lines = %v\nwant    %v", got, want)
	}
	got = nil
	if err := eachLineAt(strings.NewReader(strings.Join(lines, "")), 1000, func(off int64, line []byte) bool {
		got = append(got, seen{off, len(line)})
		return true
	}); err != nil {
		t.Fatal(err)
	}
	want = []seen{{0, 2}, {2 + 100<<10 + 1, 301}, {2 + 100<<10 + 302, 2}, {2 + 100<<10 + 304, 2}, {2 + 100<<10 + 306, 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("with the long line skipped: lines = %v\nwant    %v", got, want)
	}
}
