package discord

import (
	"context"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

// TestReplyAndEdit_ParseNoMentions: a reply, with or without images, and an
// edit tell Discord to parse no mentions, so model output carrying
// "@everyone", "@here" or a role mention pings no one.
func TestReplyAndEdit_ParseNoMentions(t *testing.T) {
	t.Parallel()
	d, rec, _ := questionAdapter(t)
	ctx := context.Background()
	const text = "@everyone @here <@&123> look"
	if _, err := d.Reply(ctx, platform.OutgoingMessage{ChatID: "C1", Text: text}); err != nil {
		t.Fatal(err)
	}
	png := []byte("\x89PNG\r\n\x1a\n")
	if _, err := d.Reply(ctx, platform.OutgoingMessage{ChatID: "C1", Text: text,
		Images: []platform.Image{{Data: png, MimeType: "image/png"}}}); err != nil {
		t.Fatal(err)
	}
	if err := d.EditMessage(ctx, "C1:M1", text); err != nil {
		t.Fatal(err)
	}
	calls := rec.recorded()
	if len(calls) != 3 {
		t.Fatalf("calls = %d, want 3", len(calls))
	}
	for i, c := range calls {
		if !strings.Contains(string(c.body), `"allowed_mentions":{"parse":[]`) {
			t.Errorf("call %d (%s %s) body %s: want allowed_mentions with parse: []", i, c.method, c.path, c.body)
		}
	}
}
