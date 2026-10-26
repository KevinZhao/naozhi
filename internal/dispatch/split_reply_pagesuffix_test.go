package dispatch

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/replyfmt"
)

var errHardLimitExceeded = errors.New("hardlimit: BASE_TYPE_MAX_LENGTH")

// hardLimitPlatform mimics a platform (e.g. Discord) whose MaxReplyLength is a
// hard API ceiling with zero headroom: any reply exceeding the limit is
// rejected outright rather than truncated. #2008.
type hardLimitPlatform struct {
	mu       sync.Mutex
	limit    int
	accepted []string
	rejected []string
}

func (h *hardLimitPlatform) Name() string                                               { return "hardlimit" }
func (h *hardLimitPlatform) RegisterRoutes(_ *http.ServeMux, _ platform.MessageHandler) {}
func (h *hardLimitPlatform) Reply(_ context.Context, msg platform.OutgoingMessage) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if utf8.RuneCountInString(msg.Text) > h.limit {
		h.rejected = append(h.rejected, msg.Text)
		return "", errHardLimitExceeded
	}
	h.accepted = append(h.accepted, msg.Text)
	return "ok", nil
}
func (h *hardLimitPlatform) EditMessage(_ context.Context, _, _ string) error { return nil }
func (h *hardLimitPlatform) MaxReplyLength() int                              { return h.limit }
func (h *hardLimitPlatform) SupportsInterimMessages() bool                    { return false }

// TestSendSplitReply_ChunksRespectHardLimit reproduces #2008: a >limit reply
// with no newline in the back half of the first window must still split into
// chunks that each stay within the platform limit AFTER the "\n— [i/N]" page
// suffix is appended.
func TestSendSplitReply_ChunksRespectHardLimit(t *testing.T) {
	const limit = 2000
	hp := &hardLimitPlatform{limit: limit}
	d := &Dispatcher{}

	// 2008 runes of contiguous non-newline text (e.g. a long CLI code line or
	// Chinese paragraph). Use ASCII 'a' so rune count == byte count for clarity.
	text := makeRunes('a', 2008)

	d.SendSplitReply(context.Background(), hp, ReplyDest{ChatID: "chat-1"}, text)

	hp.mu.Lock()
	defer hp.mu.Unlock()

	if len(hp.rejected) != 0 {
		t.Fatalf("platform rejected %d oversized chunk(s); want 0", len(hp.rejected))
	}
	if len(hp.accepted) < 2 {
		t.Fatalf("expected the reply to be split into >=2 chunks, got %d", len(hp.accepted))
	}
	for i, c := range hp.accepted {
		if n := utf8.RuneCountInString(c); n > limit {
			t.Errorf("chunk %d has %d runes, exceeds hard limit %d", i+1, n, limit)
		}
	}
	// No content must be lost: stripping the page suffixes must recover the
	// original text.
	if joined := stripPageSuffixes(hp.accepted); joined != text {
		t.Errorf("reassembled text length %d != original %d (content lost/duplicated)",
			utf8.RuneCountInString(joined), utf8.RuneCountInString(text))
	}
}

// TestSendSplitReply_TinyMaxLenSuppressesSuffix reproduces #2057: a platform
// (mis)configured with maxLen smaller than the page suffix width leaves no
// room to reserve for "[i/N]". The old code fell back to splitLen=maxLen but
// still appended the suffix, so every chunk exceeded maxLen. The fix
// suppresses the suffix in that regime; chunks must stay <= maxLen.
func TestSendSplitReply_TinyMaxLenSuppressesSuffix(t *testing.T) {
	const limit = 5 // < replyfmt page-suffix width for 1 digit (8)
	hp := &hardLimitPlatform{limit: limit}
	d := &Dispatcher{}

	text := makeRunes('a', 23) // > limit, forces multi-chunk split

	d.SendSplitReply(context.Background(), hp, ReplyDest{ChatID: "chat-1"}, text)

	hp.mu.Lock()
	defer hp.mu.Unlock()

	if len(hp.rejected) != 0 {
		t.Fatalf("platform rejected %d oversized chunk(s); want 0 (#2057)", len(hp.rejected))
	}
	if len(hp.accepted) < 2 {
		t.Fatalf("expected multi-chunk split, got %d chunks", len(hp.accepted))
	}
	for i, c := range hp.accepted {
		if n := utf8.RuneCountInString(c); n > limit {
			t.Errorf("chunk %d has %d runes, exceeds limit %d (#2057)", i+1, n, limit)
		}
		// Suffix must be suppressed: no "\n— [" marker present.
		if lastPageSuffixIndex(c) >= 0 {
			t.Errorf("chunk %d unexpectedly carries a page suffix at tiny maxLen (#2057): %q", i+1, c)
		}
	}
	// With suffix suppressed, plain concatenation must equal the original.
	var joined string
	for _, c := range hp.accepted {
		joined += c
	}
	if joined != text {
		t.Errorf("reassembled text != original (content lost) (#2057)")
	}
}

// TestSendSplitReply_SingleUseTokenCollapsesToOneMessage reproduces #2136: a
// reply longer than the platform limit must be delivered as a SINGLE
// (truncated) message for a single-use-token platform, not fanned into N
// chunks where chunks 2..N hit the consumed token and are silently lost.
func TestSendSplitReply_SingleUseTokenCollapsesToOneMessage(t *testing.T) {
	const limit = 100
	sp := &fakeSingleUseReactorless{limit: limit}
	d := &Dispatcher{}

	text := makeRunes('a', 350) // > limit, would normally split into 4 chunks

	d.SendSplitReply(context.Background(), sp, ReplyDest{ChatID: "user-1"}, text)

	if n := sp.replyCount(); n != 1 {
		t.Fatalf("expected exactly 1 send (single-use token), got %d", n)
	}
	got := sp.replies[0].Text
	if n := utf8.RuneCountInString(got); n > limit {
		t.Errorf("collapsed message has %d runes, exceeds limit %d", n, limit)
	}
	if !strings.HasSuffix(got, replyfmt.SingleReplyTruncMarker) {
		t.Errorf("collapsed message missing truncation marker: %q", got)
	}
}

// TestSendSplitReply_SingleUseTokenShortReplyUnchanged verifies a reply that
// already fits the limit is delivered verbatim (no truncation marker) on a
// single-use-token platform. #2136.
func TestSendSplitReply_SingleUseTokenShortReplyUnchanged(t *testing.T) {
	const limit = 100
	sp := &fakeSingleUseReactorless{limit: limit}
	d := &Dispatcher{}

	text := makeRunes('b', 40) // <= limit

	d.SendSplitReply(context.Background(), sp, ReplyDest{ChatID: "user-1"}, text)

	if n := sp.replyCount(); n != 1 {
		t.Fatalf("expected 1 send, got %d", n)
	}
	if got := sp.replies[0].Text; got != text {
		t.Errorf("short reply altered: got %q want %q", got, text)
	}
}

func makeRunes(r rune, n int) string {
	buf := make([]rune, n)
	for i := range buf {
		buf[i] = r
	}
	return string(buf)
}

// stripPageSuffixes removes the trailing "\n— [i/N]" page suffix from each
// chunk and concatenates the bodies.
func stripPageSuffixes(chunks []string) string {
	var out string
	for _, c := range chunks {
		if idx := lastPageSuffixIndex(c); idx >= 0 {
			c = c[:idx]
		}
		out += c
	}
	return out
}

func lastPageSuffixIndex(s string) int {
	// Suffix shape: "\n— [<i>/<N>]". Find the last "\n— [" marker.
	const marker = "\n— ["
	for i := len(s) - len(marker); i >= 0; i-- {
		if s[i:i+len(marker)] == marker {
			return i
		}
	}
	return -1
}
