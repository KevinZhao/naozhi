package dispatch

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// formatEventLine's tool_use arm delegates to formatToolUse, whose per-tool
// rendering TestFormatToolUse covers; one tool row here pins the delegation.
func TestFormatEventLine(t *testing.T) {
	t.Parallel()
	block := func(b clievent.ContentBlock) clievent.Event {
		return clievent.Event{Type: "assistant", Message: &clievent.AssistantMessage{Content: []clievent.ContentBlock{b}}}
	}
	readInput, _ := json.Marshal(map[string]string{"file_path": "/home/user/project/src/main.go"})
	for _, tc := range []struct {
		name string
		ev   clievent.Event
		want string
	}{
		{"thinking", block(clievent.ContentBlock{Type: "thinking", Text: "Let me analyze"}), "💭 Let me analyze"},
		{"empty thinking", block(clievent.ContentBlock{Type: "thinking"}), ""},
		{"tool use", block(clievent.ContentBlock{Type: "tool_use", Name: "Read", Input: readInput}), "📖 src/main.go"},
		{"no message", clievent.Event{Type: "assistant"}, ""},
	} {
		if got := formatEventLine(tc.ev); got != tc.want {
			t.Errorf("%s: formatEventLine = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestAppendStatusLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   []string
		want []string
	}{
		{[]string{"🔧 Read", "🔧 Edit"}, []string{"🔧 Read", "🔧 Edit"}},
		// consecutive thinking lines collapse to the latest
		{[]string{"💭 first", "💭 second"}, []string{"💭 second"}},
		{[]string{"💭 a", "🔧 Read", "💭 b"}, []string{"💭 a", "🔧 Read", "💭 b"}},
	} {
		var lines []string
		for _, l := range tc.in {
			lines = appendStatusLine(lines, l)
		}
		if strings.Join(lines, "\n") != strings.Join(tc.want, "\n") {
			t.Errorf("appendStatusLine(%q) = %q, want %q", tc.in, lines, tc.want)
		}
	}
	var lines []string
	for range 20 {
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
	ev := clievent.Event{
		Type:    "assistant",
		Message: &clievent.AssistantMessage{Content: []clievent.ContentBlock{{Type: "tool_use", Name: "TodoWrite", Input: input}}},
	}
	text, ok := extractTodoMessage(ev)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !strings.Contains(text, "☐ 写测试") || !strings.Contains(text, "▶ 正在跑测试") {
		t.Errorf("rendered text missing expected items: %q", text)
	}

	other := clievent.Event{
		Type:    "assistant",
		Message: &clievent.AssistantMessage{Content: []clievent.ContentBlock{{Type: "tool_use", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}}},
	}
	if _, ok := extractTodoMessage(other); ok {
		t.Error("non-TodoWrite tool should not match")
	}

	empty := clievent.Event{
		Type:    "assistant",
		Message: &clievent.AssistantMessage{Content: []clievent.ContentBlock{{Type: "tool_use", Name: "TodoWrite", Input: json.RawMessage(`{}`)}}},
	}
	if _, ok := extractTodoMessage(empty); ok {
		t.Error("empty todos should not match")
	}

	if _, ok := extractTodoMessage(clievent.Event{Type: "assistant"}); ok {
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
