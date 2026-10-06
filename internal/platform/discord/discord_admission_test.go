package discord

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/naozhi/naozhi/internal/platform"
)

var _ platform.Admitter = (*Discord)(nil)

// cdnStub answers every attachment download with a tiny PNG and counts them.
type cdnStub struct{ hits atomic.Int32 }

func (c *cdnStub) RoundTrip(*http.Request) (*http.Response, error) {
	c.hits.Add(1)
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"image/png"}},
		Body:       io.NopCloser(bytes.NewReader(png)),
	}, nil
}

// TestOnMessageCreate_AdmissionBeforeAttachmentDownload pins #3513 for
// Discord: a sender the dispatcher would refuse costs no CDN download, while
// messages with nothing to download skip the pre-check and reach the handler.
func TestOnMessageCreate_AdmissionBeforeAttachmentDownload(t *testing.T) {
	cdn := &cdnStub{}

	image := []*discordgo.MessageAttachment{{
		URL:         "https://cdn.discordapp.com/attachments/1/2/a.png",
		ContentType: "image/png",
	}}
	// video costs no download: it is refused from its metadata.
	video := []*discordgo.MessageAttachment{{
		URL:         "https://cdn.discordapp.com/attachments/1/2/a.mp4",
		Filename:    "a.mp4",
		ContentType: "video/mp4",
	}}
	refuse := func(context.Context, platform.IncomingMessage) bool { return false }
	allow := func(context.Context, platform.IncomingMessage) bool { return true }
	// mentionGate mirrors Dispatcher.Admit's group gate.
	mentionGate := func(_ context.Context, m platform.IncomingMessage) bool {
		return m.ChatType != "group" || m.MentionMe
	}
	cases := []struct {
		name        string
		admit       platform.AdmitFunc // nil: SetAdmission never called
		attachments []*discordgo.MessageAttachment
		content     string
		guild       bool // guild channel; mentioned adds an @bot mention
		mentioned   bool
		wantAdmits  int32
		wantHits    int32
		wantHandled int
		wantImages  int
	}{
		{"refused image", refuse, image, "", false, false, 1, 0, 0, 0},
		{"refused image with caption", refuse, image, "look", false, false, 1, 0, 0, 0},
		{"admitted image", allow, image, "", false, false, 1, 1, 1, 1},
		{"refused text only", refuse, nil, "hi", false, false, 0, 0, 1, 0},
		{"refused undownloadable file", refuse, video, "", false, false, 0, 0, 1, 0},
		{"no admitter", nil, image, "", false, false, 0, 1, 1, 1},
		{"unmentioned guild image", mentionGate, image, "", true, false, 1, 0, 0, 0},
		{"mentioned guild image", mentionGate, image, "<@bot123> look", true, true, 1, 1, 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cdn.hits.Store(0)
			d := New(Config{BotToken: "test-token"})
			d.cdnHTTP = &http.Client{Transport: cdn}
			setTestBotID(d, "bot123")
			var admits atomic.Int32
			var gotAdmit platform.IncomingMessage
			if tc.admit != nil {
				d.SetAdmission(func(ctx context.Context, msg platform.IncomingMessage) bool {
					admits.Add(1)
					gotAdmit = msg
					return tc.admit(ctx, msg)
				})
			}
			var mu sync.Mutex
			var handled []platform.IncomingMessage
			d.handler = func(_ context.Context, msg platform.IncomingMessage) {
				mu.Lock()
				handled = append(handled, msg)
				mu.Unlock()
			}
			m := &discordgo.Message{
				ID:          "m1",
				Author:      &discordgo.User{ID: "stranger"},
				Content:     tc.content,
				ChannelID:   "dm1",
				Attachments: tc.attachments,
			}
			wantChatType := "direct"
			if tc.guild {
				m.GuildID, m.ChannelID, wantChatType = "g1", "c1", "group"
			}
			if tc.mentioned {
				m.Mentions = []*discordgo.User{{ID: "bot123"}}
			}
			d.onMessageCreate(nil, &discordgo.MessageCreate{Message: m})
			d.dispatch.Wait()

			if got := admits.Load(); got != tc.wantAdmits {
				t.Errorf("admit calls = %d, want %d", got, tc.wantAdmits)
			}
			if tc.wantAdmits > 0 && (gotAdmit.UserID != "stranger" || gotAdmit.ChatType != wantChatType ||
				gotAdmit.MentionMe != tc.mentioned) {
				t.Errorf("admit saw user=%q chat_type=%q mention=%v, want stranger/%s/%v",
					gotAdmit.UserID, gotAdmit.ChatType, gotAdmit.MentionMe, wantChatType, tc.mentioned)
			}
			if got := cdn.hits.Load(); got != tc.wantHits {
				t.Errorf("CDN downloads = %d, want %d", got, tc.wantHits)
			}
			if len(handled) != tc.wantHandled {
				t.Fatalf("handler calls = %d, want %d", len(handled), tc.wantHandled)
			}
			if tc.wantHandled > 0 && len(handled[0].Images) != tc.wantImages {
				t.Errorf("handled images = %d, want %d", len(handled[0].Images), tc.wantImages)
			}
		})
	}
}
