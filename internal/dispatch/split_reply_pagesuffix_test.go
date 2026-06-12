package dispatch

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/platform"
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

	d.SendSplitReply(context.Background(), hp, "chat-1", text)

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
