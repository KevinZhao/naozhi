package wireup

import (
	"slices"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/planner/scout"
)

// TestCalledTools: each tool once, in first-call order; an email_read that
// passed markAs is listed with scout.MarkAsSuffix, one without it is not.
func TestCalledTools(t *testing.T) {
	const read = "mcp__aws-outlook-mcp__email_read"
	use := func(tool, detail string) clievent.EventEntry {
		return clievent.EventEntry{Type: clievent.KindToolUse, Tool: tool, Detail: detail}
	}
	for _, tc := range []struct {
		name    string
		entries []clievent.EventEntry
		want    []string
	}{
		{"plain reads", []clievent.EventEntry{
			use("mcp__aws-outlook-mcp__email_inbox", `{"limit":20}`),
			use(read, `{"messageId":"m1"}`),
			use(read, `{"messageId":"m2"}`),
			{Type: clievent.KindText, Tool: "ignored"},
		}, []string{"mcp__aws-outlook-mcp__email_inbox", read}},
		{"markAs", []clievent.EventEntry{
			use(read, `{"messageId":"m1"}`),
			use(read, `{"messageId":"m2","markAs":"read"}`),
		}, []string{read, read + scout.MarkAsSuffix}},
	} {
		if got := calledTools(tc.entries); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
