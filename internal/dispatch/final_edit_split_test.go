package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

const discordLikeLimit = 2000

var errEditTooLong = errors.New("discord-like: 400 BASE_TYPE_MAX_LENGTH")

// discordLikePlatform has interim edits and a hard 2000-rune ceiling on both
// sends and edits, like Discord. failEdits makes every answer edit fail for
// some other reason; the answered-below marker edit still succeeds.
type discordLikePlatform struct {
	fakePlatform
	failEdits bool

	emu         sync.Mutex
	editTexts   []string
	editsFailed int
}

func (p *discordLikePlatform) MaxReplyLength() int { return discordLikeLimit }

func (p *discordLikePlatform) Reply(ctx context.Context, msg platform.OutgoingMessage) (string, error) {
	if utf8.RuneCountInString(msg.Text) > discordLikeLimit {
		return "", errEditTooLong
	}
	return p.fakePlatform.Reply(ctx, msg)
}

func (p *discordLikePlatform) EditMessage(_ context.Context, _, text string) error {
	p.emu.Lock()
	defer p.emu.Unlock()
	if utf8.RuneCountInString(text) > discordLikeLimit || (p.failEdits && text != bannerAnsweredBelow) {
		p.editsFailed++
		return errEditTooLong
	}
	p.editTexts = append(p.editTexts, text)
	return nil
}

// edited returns the accepted edit texts and the number of rejected edits.
func (p *discordLikePlatform) edited() ([]string, int) {
	p.emu.Lock()
	defer p.emu.Unlock()
	return append([]string(nil), p.editTexts...), p.editsFailed
}

// longAnswer is a multi-paragraph answer of about n runes.
func longAnswer(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "paragraph %d: the quick brown fox jumps over the lazy dog.\n", i)
	}
	return b.String()
}

// assertPages checks that msgs are pages [1/N]..[N/N] within the limit and
// that stripping the suffixes gives text back.
func assertPages(t *testing.T, msgs []string, text string) {
	t.Helper()
	if len(msgs) < 2 {
		t.Fatalf("got %d message(s), want the answer split into >=2", len(msgs))
	}
	for i, m := range msgs {
		if n := utf8.RuneCountInString(m); n > discordLikeLimit {
			t.Errorf("message %d has %d runes, over the %d limit", i+1, n, discordLikeLimit)
		}
		if want := fmt.Sprintf("[%d/%d]", i+1, len(msgs)); !strings.HasSuffix(m, want) {
			t.Errorf("message %d ends %q, want suffix %q", i+1, m[max(0, len(m)-20):], want)
		}
	}
	if got := stripPageSuffixes(msgs); got != text {
		t.Errorf("reassembled answer has %d runes, want %d", utf8.RuneCountInString(got), utf8.RuneCountInString(text))
	}
}

// TestReplyIntoBanner_LongAnswerEditsFirstChunk: a 5000-rune answer puts page
// 1 in the banner and sends pages 2..N, every one within the limit.
func TestReplyIntoBanner_LongAnswerEditsFirstChunk(t *testing.T) {
	t.Parallel()
	p := &discordLikePlatform{}
	text := longAnswer(5000)

	(&Dispatcher{}).replyIntoBanner(context.Background(), p, "chat-1", "banner-1", text)

	edits, failed := p.edited()
	if len(edits) != 1 || failed != 0 {
		t.Fatalf("banner edits = %d ok / %d failed, want exactly 1 ok", len(edits), failed)
	}
	assertPages(t, append(edits, p.allReplies()...), text)
}

// TestReplyIntoBanner_ShortAnswerEditOnly: an answer within the limit is the
// banner edit alone, with no page suffix and no extra message.
func TestReplyIntoBanner_ShortAnswerEditOnly(t *testing.T) {
	t.Parallel()
	p := &discordLikePlatform{}
	const text = "done: all tests pass"

	(&Dispatcher{}).replyIntoBanner(context.Background(), p, "chat-1", "banner-1", text)

	if edits, _ := p.edited(); len(edits) != 1 || edits[0] != text {
		t.Errorf("banner edits = %q, want [%q]", edits, text)
	}
	if n := p.replyCount(); n != 0 {
		t.Errorf("sent %d message(s), want 0", n)
	}
}

// TestReplyIntoBanner_EditFailureSendsAllAndMarksBanner: when the answer edit
// fails for a non-length reason, every page is sent as a new message and the
// banner is replaced by the answered-below marker instead of the tool status.
func TestReplyIntoBanner_EditFailureSendsAllAndMarksBanner(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		text string
	}{
		{"one_chunk", "done: all tests pass"},
		{"many_chunks", longAnswer(5000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &discordLikePlatform{failEdits: true}

			(&Dispatcher{}).replyIntoBanner(context.Background(), p, "chat-1", "banner-1", tc.text)

			if edits, _ := p.edited(); len(edits) != 1 || edits[0] != bannerAnsweredBelow {
				t.Errorf("successful banner edits = %q, want only the marker %q", edits, bannerAnsweredBelow)
			}
			replies := p.allReplies()
			if tc.name == "one_chunk" {
				if len(replies) != 1 || replies[0] != tc.text {
					t.Errorf("replies = %q, want [%q]", replies, tc.text)
				}
				return
			}
			assertPages(t, replies, tc.text)
		})
	}
}

// singleUseBanner is a single-use-token platform that has a banner to edit.
type singleUseBanner struct {
	fakeSingleUseInterim
	edits []string
}

func (p *singleUseBanner) EditMessage(_ context.Context, _, text string) error {
	p.edits = append(p.edits, text)
	return nil
}

// TestReplyIntoBanner_SingleUseTokenSendsNothing: a single-use-token platform
// with a banner gets one truncated edit and no extra message, which would be
// rejected once the token is spent.
func TestReplyIntoBanner_SingleUseTokenSendsNothing(t *testing.T) {
	t.Parallel()
	p := &singleUseBanner{}
	limit := p.MaxReplyLength()

	(&Dispatcher{}).replyIntoBanner(context.Background(), p, "chat-1", "banner-1", longAnswer(3*limit))

	if n := p.replyCount(); n != 0 {
		t.Errorf("sent %d message(s) on a single-use-token platform, want 0", n)
	}
	if len(p.edits) != 1 || utf8.RuneCountInString(p.edits[0]) > limit {
		t.Fatalf("banner edits = %d, want one within %d runes", len(p.edits), limit)
	}
}

// TestIMDeliveryReply_LongAnswerWithBannerStaysWithinLimit drives the real
// imDelivery.reply on a turn that posted a progress banner: the final answer
// is split before it is edited in, instead of a full-length edit that a
// Discord-like platform rejects.
func TestIMDeliveryReply_LongAnswerWithBannerStaysWithinLimit(t *testing.T) {
	t.Parallel()
	p := &discordLikePlatform{fakePlatform: fakePlatform{supportsInterim: true, replyMsgID: "banner-1"}}
	d := newTestDispatcher(&fakePlatform{})
	d.platforms = map[string]platform.Platform{"fake": p}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	msg := incomingMsg("hello")
	o := d.newIMOrigin(msg, slog.Default(), "fake:direct:chat1:general", "general", session.AgentOpts{}, imMessage, len(msg.Text), 0)
	dl := &imDelivery{o: o, p: p, lg: slog.Default()}
	onEvent := dl.SessionReady(ctx, sessionview.SessionExisting)
	onEvent(clievent.Event{
		Type:    "assistant",
		Message: &clievent.AssistantMessage{Content: []clievent.ContentBlock{{Type: "thinking", Text: "analyzing"}}},
	})
	dl.tracker.waitReady(ctx)
	if dl.tracker.getThinkingMsgID() == "" {
		t.Fatal("no progress banner was posted")
	}
	text := longAnswer(5000)

	dl.reply(ctx, &clievent.SendResult{Text: text}, nil)
	dl.tracker.stop()

	edits, failed := p.edited()
	if failed != 0 {
		t.Errorf("%d banner edit(s) rejected by the platform limit, want 0", failed)
	}
	if len(edits) == 0 {
		t.Fatal("the answer was never edited into the banner")
	}
	// The first reply is the banner itself; the rest are answer pages.
	replies := p.allReplies()
	if len(replies) == 0 {
		t.Fatal("the banner was never posted")
	}
	assertPages(t, append([]string{edits[len(edits)-1]}, replies[1:]...), text)
}
