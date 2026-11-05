package wireup

import (
	"context"
	"slices"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/planner/scout"
	"github.com/naozhi/naozhi/internal/session"
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

// TestScoutSession_CutShortInterruptsBeforeReset: a run cut short (Stop's
// cancel) ends the CLI before the reset's graceful shutdown, which waits
// seconds for a CLI mid-turn and held naozhi's SIGTERM past Scout.Stop's
// wait.
func TestScoutSession_CutShortInterruptsBeforeReset(t *testing.T) {
	// A cron key: reconnect retires a scout key's surviving shim, and the
	// key is all ScoutSession takes from the scout.
	const key = "cron:5d0e8a3c1b7f9246"
	fs := session.StartFakeShimForTest(t, key, 1, []string{adoptE2EAssistant})
	r := session.NewRouter(fs.Config)
	t.Cleanup(r.Shutdown)
	r.ReconnectShimsCtx(context.Background())
	fs.Attached(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := ScoutSession(r)(ctx, key, session.AgentOpts{}, "patrol"); err == nil {
		t.Fatal("a cut-short run returned no error")
	}
	sent := fs.Sent()
	i, j := slices.Index(sent, "interrupt"), slices.Index(sent, "shutdown")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("client sent %q, want interrupt before shutdown", sent)
	}
}
