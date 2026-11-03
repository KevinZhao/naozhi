package cli

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/eventlog/ring"
	"github.com/naozhi/naozhi/internal/planner/activity"
)

// A turn's tool events become activities once their results say they
// succeeded, Write / Edit entries carry their root-relative path, and the
// turn-end scan adds the outputs written without a Write call.
func TestProcessActivity_Turn(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	proto := &ClaudeProtocol{}
	p := &Process{eventLog: ring.NewEventLog(32), eventCh: make(chan clievent.Event, 16), killCh: make(chan struct{})}
	var (
		mu  sync.Mutex
		got []activity.Activity
	)
	p.SetOnActivity(cwd, "", func(a activity.Activity) {
		mu.Lock()
		got = append(got, a)
		mu.Unlock()
	})
	feed := func(v any) {
		t.Helper()
		line, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		evs, _, err := proto.ReadEventInto(string(line), nil)
		if err != nil {
			t.Fatalf("ReadEventInto(%s): %v", line, err)
		}
		for _, ev := range evs {
			p.dispatchProtocolEvent(ev, slog.New(slog.DiscardHandler))
		}
	}
	toolUse := func(id, name string, input map[string]string) map[string]any {
		return map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": id, "name": name, "input": input},
		}}}
	}
	toolResult := func(id, text string, isError bool) map[string]any {
		return map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": id, "content": text, "is_error": isError},
		}}}
	}

	feed(toolUse("t1", "Write", map[string]string{"file_path": filepath.Join(cwd, "docs", "a.md"), "content": "x"}))
	feed(toolResult("t1", "File created", false))
	feed(toolUse("t2", "Write", map[string]string{"file_path": filepath.Join(cwd, "main.go"), "content": "x"}))
	feed(toolResult("t2", "File created", false))
	feed(toolUse("t3", "Bash", map[string]string{"command": "git push origin feat"}))
	feed(toolResult("t3", "rejected", true))
	feed(toolUse("t4", "Bash", map[string]string{"command": "gh pr create --title x"}))
	feed(toolResult("t4", "https://github.com/o/r/pull/12\n", false))
	feed(toolUse("t5", "Bash", map[string]string{"command": "gh pr view 12 && gh pr view 13"}))
	feed(toolResult("t5", "https://github.com/o/r/pull/12 https://github.com/o/r/pull/13", false))
	feed(toolUse("t6", "Read", map[string]string{"file_path": filepath.Join(cwd, "notes.md")}))
	feed(toolResult("t6", "https://github.com/o/r/pull/14", false))
	feed(toolUse("t7", "Bash", map[string]string{"command": "git push -u origin feat && gh pr create --title y"}))
	feed(toolResult("t7", "remote: https://github.com/o/r/pull/new/feat\nhttps://github.com/o/r/pull/15\n", false))

	// Written by a script during the turn, and an older file that was not.
	if err := os.WriteFile(filepath.Join(cwd, "chart.svg"), []byte("<svg/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(cwd, "old.csv")
	if err := os.WriteFile(old, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cwd, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "docs", "a.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	feed(map[string]any{"type": "result", "subtype": "success", "result": "done"})

	want := []activity.Activity{
		{Type: activity.TypeOutput, Summary: "docs/a.md", Path: "docs/a.md"},
		{Type: activity.TypePRCreate, Summary: "gh pr create", State: activity.StateOpened, URL: "https://github.com/o/r/pull/12"},
		{Type: activity.TypePR, Summary: "https://github.com/o/r/pull/13", URL: "https://github.com/o/r/pull/13"},
		{Type: activity.TypeGitPush, Summary: "git push -u origin feat"},
		{Type: activity.TypePRCreate, Summary: "gh pr create", State: activity.StateOpened, URL: "https://github.com/o/r/pull/15"},
		{Type: activity.TypeOutput, Summary: "chart.svg", Path: "chart.svg"},
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= len(want) || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for p.acts.scanning.Load() {
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(got, want) {
		t.Errorf("activities:\n got %+v\nwant %+v", got, want)
	}

	var paths, acts []string
	for _, e := range p.EventEntries() {
		if e.Type == clievent.KindToolUse {
			paths = append(paths, e.Path)
			acts = append(acts, e.Activity)
		}
	}
	if want := []string{"docs/a.md", "main.go", "", "", "", "", ""}; !slices.Equal(paths, want) {
		t.Errorf("tool_use paths = %q, want %q", paths, want)
	}
	// Tagged before the call runs: t3's push failed, and is not reported.
	if want := []string{"output", "", "git_push", "pr_create", "", "", "git_push,pr_create"}; !slices.Equal(acts, want) {
		t.Errorf("tool_use activities = %q, want %q", acts, want)
	}
}
