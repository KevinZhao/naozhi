package wireup

import (
	"slices"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/planner/scout"
)

// TestCalledTools: each tool once, in first-call order; an email_read that
// passed markAs is listed with scout.MarkAsSuffix, one without it is not,
// however long the input the entry's Detail cuts short.
func TestCalledTools(t *testing.T) {
	const read = "mcp__aws-outlook-mcp__email_read"
	use := func(tool, input string) clievent.EventEntry {
		ev := clievent.Event{Type: "assistant", Message: &clievent.AssistantMessage{Role: "assistant",
			Content: []clievent.ContentBlock{{Type: "tool_use", ID: "tu", Name: tool, Input: []byte(input)}}}}
		return cli.EventEntriesFromEventAt(ev, 1)[0]
	}
	// A batch read of three conversations, markAs past Detail's 300 runes.
	id := `"AAQkAD` + strings.Repeat("x", 104) + `="`
	batch := `{"conversationId":[` + id + `,` + id + `,` + id + `],"format":"markdown","latestOnly":true,"markAs":"read"}`
	for _, tc := range []struct {
		name    string
		entries []clievent.EventEntry
		want    []string
	}{
		{"plain reads", []clievent.EventEntry{
			use("mcp__aws-outlook-mcp__email_inbox", `{"limit":20}`),
			use(read, `{"conversationId":"m1"}`),
			use(read, `{"conversationId":"m2","markAs":null}`),
			{Type: clievent.KindText, Tool: "ignored"},
		}, []string{"mcp__aws-outlook-mcp__email_inbox", read}},
		{"markAs", []clievent.EventEntry{
			use(read, `{"conversationId":"m1"}`),
			use(read, `{"conversationId":"m2","markAs":"read"}`),
		}, []string{read, read + scout.MarkAsSuffix}},
		{"markAs in a long batch", []clievent.EventEntry{use(read, batch)}, []string{read + scout.MarkAsSuffix}},
	} {
		if got := calledTools(tc.entries); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
