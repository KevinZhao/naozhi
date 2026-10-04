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
