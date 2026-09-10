package cli

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestPassthroughShouldFanOut pins R20260608133928-GO-6: the passthrough
// onEvent fan-out gate must align with the legacy Send path
// (process_send.go:286-293), which only fans out for thinking/tool_use
// blocks or AskQuestion payloads.
func TestPassthroughShouldFanOut(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ev   clievent.Event
		want bool
	}{
		{
			name: "plain text-only assistant event — no fan-out",
			ev: clievent.Event{
				Type: "assistant",
				Message: &clievent.AssistantMessage{
					Content: []clievent.ContentBlock{{Type: "text", Text: "hello"}},
				},
			},
			want: false,
		},
		{
			name: "nil Message — no fan-out",
			ev: clievent.Event{
				Type:    "assistant",
				Message: nil,
			},
			want: false,
		},
		{
			name: "empty content blocks — no fan-out",
			ev: clievent.Event{
				Type:    "assistant",
				Message: &clievent.AssistantMessage{Content: []clievent.ContentBlock{}},
			},
			want: false,
		},
		{
			name: "tool_use block — fan-out",
			ev: clievent.Event{
				Type: "assistant",
				Message: &clievent.AssistantMessage{
					Content: []clievent.ContentBlock{{Type: "tool_use", Name: "Bash"}},
				},
			},
			want: true,
		},
		{
			name: "thinking block — fan-out",
			ev: clievent.Event{
				Type: "assistant",
				Message: &clievent.AssistantMessage{
					Content: []clievent.ContentBlock{{Type: "thinking", Text: "reasoning..."}},
				},
			},
			want: true,
		},
		{
			name: "AskQuestion payload present — fan-out regardless of blocks",
			ev: clievent.Event{
				Type:        "assistant",
				Message:     &clievent.AssistantMessage{Content: []clievent.ContentBlock{{Type: "text", Text: "choose"}}},
				AskQuestion: &clievent.AskQuestion{ToolUseID: "toolu_1"},
			},
			want: true,
		},
		{
			name: "AskQuestion with nil Message — fan-out",
			ev: clievent.Event{
				Type:        "assistant",
				AskQuestion: &clievent.AskQuestion{ToolUseID: "toolu_2"},
			},
			want: true,
		},
		{
			name: "mixed text + tool_use blocks — fan-out because tool_use present",
			ev: clievent.Event{
				Type: "assistant",
				Message: &clievent.AssistantMessage{
					Content: []clievent.ContentBlock{
						{Type: "text", Text: "calling tool"},
						{Type: "tool_use", Name: "Read"},
					},
				},
			},
			want: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := passthroughShouldFanOut(tc.ev)
			if got != tc.want {
				t.Errorf("passthroughShouldFanOut(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
