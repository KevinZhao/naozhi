package slack

import (
	"context"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

// TestReplyAndEdit_DefuseBroadcasts: a reply or an edit carrying Slack's
// special mentions sends them escaped, so they show as text and notify no
// one; a link and a blockquote go out unchanged.
func TestReplyAndEdit_DefuseBroadcasts(t *testing.T) {
	t.Parallel()
	s, f := newAPIAdapter(t)
	ctx := context.Background()
	const text = "<!channel> <!here> <!everyone> <!subteam^S1> see <https://x.test|x>\n> quote"
	const want = "&lt;!channel> &lt;!here> &lt;!everyone> &lt;!subteam^S1> see <https://x.test|x>\n> quote"
	if _, err := s.Reply(ctx, platform.OutgoingMessage{ChatID: "C1", Text: text}); err != nil {
		t.Fatal(err)
	}
	if err := s.EditMessage(ctx, "C1:1700000000.000100", text); err != nil {
		t.Fatal(err)
	}
	calls := f.recorded()
	if len(calls) != 2 {
		t.Fatalf("calls = %+v, want a post and an update", calls)
	}
	for _, c := range calls {
		got := c.form.Get("text")
		if got != want {
			t.Errorf("%s text = %q, want %q", c.method, got, want)
		}
		if strings.Contains(got, "<!") {
			t.Errorf("%s text %q still carries a special mention", c.method, got)
		}
	}
}
