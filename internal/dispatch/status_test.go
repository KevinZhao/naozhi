package dispatch

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

func TestFormatEventLine_Thinking(t *testing.T) {
	t.Parallel()
	ev := cli.Event{
		Type:    "assistant",
		Message: &cli.AssistantMessage{Content: []cli.ContentBlock{{Type: "thinking", Text: "Let me analyze the code structure"}}},
	}
	got := formatEventLine(ev)
	if !strings.HasPrefix(got, "💭") {
		t.Errorf("expected thinking prefix, got %q", got)
	}
	if !strings.Contains(got, "analyze") {
		t.Errorf("expected thinking summary, got %q", got)
	}
}

func TestFormatEventLine_ThinkingEmpty(t *testing.T) {
	t.Parallel()
	ev := cli.Event{
		Type:    "assistant",
		Message: &cli.AssistantMessage{Content: []cli.ContentBlock{{Type: "thinking", Text: ""}}},
	}
	got := formatEventLine(ev)
	if got != "" {
		t.Errorf("expected empty for empty thinking, got %q", got)
	}
}

func TestFormatEventLine_ToolUse_Read(t *testing.T) {
	t.Parallel()
	input, _ := json.Marshal(map[string]string{"file_path": "/home/user/project/src/main.go"})
	ev := cli.Event{
		Type:    "assistant",
		Message: &cli.AssistantMessage{Content: []cli.ContentBlock{{Type: "tool_use", Name: "Read", Input: input}}},
	}
	got := formatEventLine(ev)
	if got != "📖 src/main.go" {
		t.Errorf("got %q, want '📖 src/main.go'", got)
	}
}

func TestFormatEventLine_ToolUse_Bash(t *testing.T) {
	t.Parallel()
	input, _ := json.Marshal(map[string]string{"command": "go test ./..."})
	ev := cli.Event{
		Type:    "assistant",
		Message: &cli.AssistantMessage{Content: []cli.ContentBlock{{Type: "tool_use", Name: "Bash", Input: input}}},
	}
	got := formatEventLine(ev)
	if got != "⚡ go test ./..." {
		t.Errorf("got %q", got)
	}
}

func TestFormatEventLine_ToolUse_Agent(t *testing.T) {
	t.Parallel()
	input, _ := json.Marshal(map[string]string{"description": "review code changes"})
	ev := cli.Event{
		Type:    "assistant",
		Message: &cli.AssistantMessage{Content: []cli.ContentBlock{{Type: "tool_use", Name: "Agent", Input: input}}},
	}
	got := formatEventLine(ev)
	if got != "🤖 review code changes" {
		t.Errorf("got %q", got)
	}
}

func TestFormatEventLine_ToolUse_Unknown(t *testing.T) {
	t.Parallel()
	ev := cli.Event{
		Type:    "assistant",
		Message: &cli.AssistantMessage{Content: []cli.ContentBlock{{Type: "tool_use", Name: "CustomTool"}}},
	}
	got := formatEventLine(ev)
	if got != "🔧 CustomTool" {
		t.Errorf("got %q", got)
	}
}

func TestFormatEventLine_NoMessage(t *testing.T) {
	t.Parallel()
	ev := cli.Event{Type: "assistant"}
	if got := formatEventLine(ev); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestAppendStatusLine_Basic(t *testing.T) {
	t.Parallel()
	var lines []string
	lines = appendStatusLine(lines, "🔧 Read")
	lines = appendStatusLine(lines, "🔧 Edit")
	if len(lines) != 2 {
		t.Fatalf("len = %d, want 2", len(lines))
	}
}

func TestAppendStatusLine_CollapseThinking(t *testing.T) {
	t.Parallel()
	var lines []string
	lines = appendStatusLine(lines, "💭 first thought")
	lines = appendStatusLine(lines, "💭 second thought")
	if len(lines) != 1 {
		t.Fatalf("len = %d, want 1 (thinking collapsed)", len(lines))
	}
	if lines[0] != "💭 second thought" {
		t.Errorf("got %q, want second thought", lines[0])
	}
}

func TestAppendStatusLine_ThinkingThenTool(t *testing.T) {
	t.Parallel()
	var lines []string
	lines = appendStatusLine(lines, "💭 thinking")
	lines = appendStatusLine(lines, "🔧 Read")
	lines = appendStatusLine(lines, "💭 more thinking")
	if len(lines) != 3 {
		t.Fatalf("len = %d, want 3", len(lines))
	}
}

func TestAppendStatusLine_MaxLines(t *testing.T) {
	t.Parallel()
	var lines []string
	for i := 0; i < 20; i++ {
		lines = appendStatusLine(lines, "🔧 tool")
	}
	if len(lines) > maxStatusLines {
		t.Errorf("len = %d, should be <= %d", len(lines), maxStatusLines)
	}
}

func TestExtractTodoMessage(t *testing.T) {
	t.Parallel()
	input, _ := json.Marshal(map[string]any{
		"todos": []map[string]any{
			{"content": "写测试", "status": "pending"},
			{"content": "跑测试", "status": "in_progress", "activeForm": "正在跑测试"},
		},
	})
	ev := cli.Event{
		Type:    "assistant",
		Message: &cli.AssistantMessage{Content: []cli.ContentBlock{{Type: "tool_use", Name: "TodoWrite", Input: input}}},
	}
	text, ok := extractTodoMessage(ev)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !strings.Contains(text, "☐ 写测试") || !strings.Contains(text, "▶ 正在跑测试") {
		t.Errorf("rendered text missing expected items: %q", text)
	}

	other := cli.Event{
		Type:    "assistant",
		Message: &cli.AssistantMessage{Content: []cli.ContentBlock{{Type: "tool_use", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}}},
	}
	if _, ok := extractTodoMessage(other); ok {
		t.Error("non-TodoWrite tool should not match")
	}

	empty := cli.Event{
		Type:    "assistant",
		Message: &cli.AssistantMessage{Content: []cli.ContentBlock{{Type: "tool_use", Name: "TodoWrite", Input: json.RawMessage(`{}`)}}},
	}
	if _, ok := extractTodoMessage(empty); ok {
		t.Error("empty todos should not match")
	}

	if _, ok := extractTodoMessage(cli.Event{Type: "assistant"}); ok {
		t.Error("nil message should not match")
	}
}

func TestShortenPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input, want string
	}{
		{"/home/user/project/src/main.go", "src/main.go"},
		{"/main.go", "main.go"},
		{"file.go", "file.go"},
	}
	for _, tt := range tests {
		got := shortenPath(tt.input)
		if got != tt.want {
			t.Errorf("shortenPath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
