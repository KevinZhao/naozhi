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

// TestSendSplitReply_NewlineDenseRespectsHardLimit reproduces #2056: a reply
// dense with newlines makes SplitText break early (chunk ~= splitWidth/2),
// so the true chunk count can be ~2x the naive ceil(runeCount/splitWidth)
// estimate. When the true count crosses a decimal digit boundary the page
// suffix widens by 2 runes beyond what was reserved, pushing chunk+suffix
// over a zero-headroom hard limit. Every emitted chunk must stay <= limit.
func TestSendSplitReply_NewlineDenseRespectsHardLimit(t *testing.T) {
	const limit = 2000
	hp := &hardLimitPlatform{limit: limit}
	d := &Dispatcher{}

	// Lines sized so SplitText packs whole lines but the newline-driven
	// breaks make the TRUE chunk count cross a decimal digit boundary
	// (>=10) while the naive ceil(runeCount/splitWidth) estimate stays
	// single-digit. With the pre-#2056 estimate this reserves an 8-rune
	// suffix budget but the real suffix is 10 runes, so a full splitLen
	// chunk + suffix reaches 2002 > 2000. (996 'a' + '\n' = 997 runes per
	// line, 14 lines → runeCount=13958, naive est 9, real total 10.)
	line := makeRunes('a', 996) + "\n" // 997 runes per line
	var b []byte
	for i := 0; i < 14; i++ {
		b = append(b, line...)
	}
	text := string(b) // 13958 runes, newline-dense

	d.SendSplitReply(context.Background(), hp, "chat-1", text)

	hp.mu.Lock()
	defer hp.mu.Unlock()

	if len(hp.rejected) != 0 {
		t.Fatalf("platform rejected %d oversized chunk(s); want 0 (#2056)", len(hp.rejected))
	}
	if len(hp.accepted) < 10 {
		t.Fatalf("expected newline-dense reply to split into >=10 chunks, got %d", len(hp.accepted))
	}
	for i, c := range hp.accepted {
		if n := utf8.RuneCountInString(c); n > limit {
			t.Errorf("chunk %d has %d runes, exceeds hard limit %d (#2056)", i+1, n, limit)
		}
	}
	if joined := stripPageSuffixes(hp.accepted); joined != text {
		t.Errorf("reassembled text != original (content lost/duplicated) (#2056)")
	}
}

// TestSendSplitReply_TinyMaxLenSuppressesSuffix reproduces #2057: a platform
// (mis)configured with maxLen smaller than the page suffix width leaves no
// room to reserve for "[i/N]". The old code fell back to splitLen=maxLen but
// still appended the suffix, so every chunk exceeded maxLen. The fix
// suppresses the suffix in that regime; chunks must stay <= maxLen.
func TestSendSplitReply_TinyMaxLenSuppressesSuffix(t *testing.T) {
	const limit = 5 // < pageSuffixRuneWidth(1) == 8
	hp := &hardLimitPlatform{limit: limit}
	d := &Dispatcher{}

	text := makeRunes('a', 23) // > limit, forces multi-chunk split

	d.SendSplitReply(context.Background(), hp, "chat-1", text)

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

// singleTokenPlatform mimics a platform (e.g. WeChat iLink) whose reply is
// authorised by a single-use context token: the FIRST send succeeds, every
// subsequent send in the same turn is rejected (token already consumed).
// #2136.
type singleTokenPlatform struct {
	mu       sync.Mutex
	limit    int
	accepted []string
	sends    int
}

func (s *singleTokenPlatform) Name() string                                               { return "singletoken" }
func (s *singleTokenPlatform) RegisterRoutes(_ *http.ServeMux, _ platform.MessageHandler) {}
func (s *singleTokenPlatform) Reply(_ context.Context, msg platform.OutgoingMessage) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sends++
	if s.sends > 1 {
		// Token already consumed by the first send — upstream rejects reuse.
		return "", errors.New("singletoken: context_token already used")
	}
	s.accepted = append(s.accepted, msg.Text)
	return "ok", nil
}
func (s *singleTokenPlatform) EditMessage(_ context.Context, _, _ string) error { return nil }
func (s *singleTokenPlatform) MaxReplyLength() int                              { return s.limit }
func (s *singleTokenPlatform) SupportsInterimMessages() bool                    { return false }
func (s *singleTokenPlatform) UsesSingleUseReplyToken() bool                    { return true }

// TestSendSplitReply_SingleUseTokenCollapsesToOneMessage reproduces #2136: a
// reply longer than the platform limit must be delivered as a SINGLE
// (truncated) message for a single-use-token platform, not fanned into N
// chunks where chunks 2..N hit the consumed token and are silently lost.
func TestSendSplitReply_SingleUseTokenCollapsesToOneMessage(t *testing.T) {
	const limit = 100
	sp := &singleTokenPlatform{limit: limit}
	d := &Dispatcher{}

	text := makeRunes('a', 350) // > limit, would normally split into 4 chunks

	d.SendSplitReply(context.Background(), sp, "user-1", text)

	sp.mu.Lock()
	defer sp.mu.Unlock()

	if sp.sends != 1 {
		t.Fatalf("expected exactly 1 send (single-use token), got %d", sp.sends)
	}
	if len(sp.accepted) != 1 {
		t.Fatalf("expected 1 accepted message, got %d", len(sp.accepted))
	}
	if n := utf8.RuneCountInString(sp.accepted[0]); n > limit {
		t.Errorf("collapsed message has %d runes, exceeds limit %d", n, limit)
	}
	if !strings.HasSuffix(sp.accepted[0], singleReplyTruncMarker) {
		t.Errorf("collapsed message missing truncation marker: %q", sp.accepted[0])
	}
}

// TestSendSplitReply_SingleUseTokenShortReplyUnchanged verifies a reply that
// already fits the limit is delivered verbatim (no truncation marker) on a
// single-use-token platform. #2136.
func TestSendSplitReply_SingleUseTokenShortReplyUnchanged(t *testing.T) {
	const limit = 100
	sp := &singleTokenPlatform{limit: limit}
	d := &Dispatcher{}

	text := makeRunes('b', 40) // <= limit

	d.SendSplitReply(context.Background(), sp, "user-1", text)

	sp.mu.Lock()
	defer sp.mu.Unlock()

	if sp.sends != 1 || len(sp.accepted) != 1 {
		t.Fatalf("expected 1 send/1 accepted, got sends=%d accepted=%d", sp.sends, len(sp.accepted))
	}
	if sp.accepted[0] != text {
		t.Errorf("short reply altered: got %q want %q", sp.accepted[0], text)
	}
}

// TestSendSplitReply_ByteFastPath_ShortAsciiSingleChunk verifies the
// R202606j-PERF-006 byte-length fast path: an ASCII reply whose byte length is
// <= maxLen is sent as a single verbatim chunk (no page suffix), matching the
// pre-optimisation behaviour.
func TestSendSplitReply_ByteFastPath_ShortAsciiSingleChunk(t *testing.T) {
	const limit = 2000
	hp := &hardLimitPlatform{limit: limit}
	d := &Dispatcher{}

	text := makeRunes('a', 100) // len(text)=100 bytes <= maxLen

	d.SendSplitReply(context.Background(), hp, "chat-1", text)

	hp.mu.Lock()
	defer hp.mu.Unlock()
	if len(hp.accepted) != 1 {
		t.Fatalf("expected 1 chunk via byte fast path, got %d", len(hp.accepted))
	}
	if hp.accepted[0] != text {
		t.Errorf("fast-path chunk altered: got %q want %q", hp.accepted[0], text)
	}
	if lastPageSuffixIndex(hp.accepted[0]) >= 0 {
		t.Errorf("fast-path single chunk unexpectedly carries a page suffix: %q", hp.accepted[0])
	}
}

// TestSendSplitReply_ByteFastPath_MultibyteFallthroughSingleChunk guards the
// fast-path boundary: a multibyte reply whose BYTE length exceeds maxLen but
// whose RUNE count still fits (so it should not split) must fall through the
// byte fast path and still be delivered as a single chunk by the rune-count
// path. This confirms the fast path's len()<=maxLen guard is conservative
// (never over-fires) and that the slow path remains correct for such inputs.
func TestSendSplitReply_ByteFastPath_MultibyteFallthroughSingleChunk(t *testing.T) {
	const limit = 2000
	hp := &hardLimitPlatform{limit: limit}
	d := &Dispatcher{}

	// 1500 CJK runes: rune count 1500 <= 2000 (no split needed), but byte
	// length is 4500 (3 bytes/rune) > maxLen, so the byte fast path is skipped.
	text := makeRunes('好', 1500)
	if len(text) <= limit {
		t.Fatalf("test setup: expected byte length > limit, got %d", len(text))
	}

	d.SendSplitReply(context.Background(), hp, "chat-1", text)

	hp.mu.Lock()
	defer hp.mu.Unlock()
	if len(hp.rejected) != 0 {
		t.Fatalf("platform rejected %d chunk(s); want 0", len(hp.rejected))
	}
	if len(hp.accepted) != 1 {
		t.Fatalf("expected 1 chunk (rune count fits), got %d", len(hp.accepted))
	}
	if hp.accepted[0] != text {
		t.Errorf("single chunk altered: got %d-rune chunk want original", utf8.RuneCountInString(hp.accepted[0]))
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
