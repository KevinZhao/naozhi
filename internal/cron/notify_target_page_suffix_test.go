package cron

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/replyfmt"
)

// The gap J2 (#2548) closed: a cron notification split across several messages
// arrived with no "[2/4]" marker, while the IM reply path has carried one since
// #2008. The recipient of a four-part job result could not tell whether they had
// the whole thing, or which part they were looking at — and cron drops the tail on
// a chunk-count cap (#568) or a mid-flush deadline (#799), so "is something
// missing" is a question that actually comes up.
//
// dispatch had pageSuffixRuneWidth / upperBoundChunks and cron had neither;
// internal/replyfmt now holds them for both. cron cannot import internal/platform
// (#725) or dispatch, which is why a third package rather than a call into either.

var pageSuffixRe = regexp.MustCompile(`\n— \[(\d+)/(\d+)\]$`)

// TestNotifyTarget_MultiChunkCarriesPageSuffix: every chunk of a split
// notification ends with its own "[i/N]", numbered 1..N in order.
func TestNotifyTarget_MultiChunkCarriesPageSuffix(t *testing.T) {
	t.Parallel()
	fp := &fakePartialPlatform{failAt: 1000, maxLen: 40}
	s := &Scheduler{}
	storeFakeNotifySender(s, map[string]platform.Platform{"fake-notify": fp})

	// Three chunks: under the cap, so nothing is dropped and every suffix is
	// observable.
	long := buildDistinctChunks(3, 40)
	s.notifyTarget("fake-notify", "chat-x", long)

	sent := fp.sentChunks()
	if len(sent) < 2 {
		t.Fatalf("fixture produced %d chunk(s); need at least 2 for a page suffix to appear", len(sent))
	}
	for i, chunk := range sent {
		m := pageSuffixRe.FindStringSubmatch(chunk)
		if m == nil {
			t.Errorf("chunk %d has no page suffix; recipients of a split cron notify "+
				"cannot tell how many parts there are:\n%q", i+1, chunk)
			continue
		}
		if want := itoa(i + 1); m[1] != want {
			t.Errorf("chunk %d numbered %q, want %q", i+1, m[1], want)
		}
		if want := itoa(len(sent)); m[2] != want {
			t.Errorf("chunk %d reports total %q, want %q", i+1, m[2], want)
		}
	}
}

// TestNotifyTarget_SingleChunkHasNoPageSuffix: a notification that fits in one
// message must not gain a "[1/1]" — the marker exists to say "there is more".
func TestNotifyTarget_SingleChunkHasNoPageSuffix(t *testing.T) {
	t.Parallel()
	fp := &fakePartialPlatform{failAt: 1000, maxLen: 200}
	s := &Scheduler{}
	storeFakeNotifySender(s, map[string]platform.Platform{"fake-notify": fp})

	s.notifyTarget("fake-notify", "chat-x", "short result")

	sent := fp.sentChunks()
	if len(sent) != 1 {
		t.Fatalf("expected one chunk, got %d", len(sent))
	}
	if pageSuffixRe.MatchString(sent[0]) {
		t.Errorf("single-chunk notify gained a page suffix: %q", sent[0])
	}
}

// TestNotifyTarget_ChunkPlusSuffixStaysWithinMaxLen is the reason the width is
// reserved BEFORE splitting rather than appended after. Discord rejects payloads
// over its ceiling outright and the retry re-sends the same oversized bytes
// (#2008), so a chunk that only breaches the limit once the suffix is glued on is
// a message that never arrives.
func TestNotifyTarget_ChunkPlusSuffixStaysWithinMaxLen(t *testing.T) {
	t.Parallel()
	const maxLen = 40
	fp := &fakePartialPlatform{failAt: 1000, maxLen: maxLen}
	s := &Scheduler{}
	storeFakeNotifySender(s, map[string]platform.Platform{"fake-notify": fp})

	s.notifyTarget("fake-notify", "chat-x", buildDistinctChunks(4, maxLen))

	for i, chunk := range fp.sentChunks() {
		if n := utf8.RuneCountInString(chunk); n > maxLen {
			t.Errorf("chunk %d is %d runes, over the platform's %d: the page suffix must be "+
				"reserved before the split, not appended after (#2008)\n%q", i+1, n, maxLen, chunk)
		}
	}
}

// TestNotifyTarget_CapDroppedTailKeepsOriginalTotal: when the chunk-count cap
// (#568) drops a tail, the surviving chunks must still report the ORIGINAL total.
// Renumbering to the delivered subset would tell the recipient they have all of it.
func TestNotifyTarget_CapDroppedTailKeepsOriginalTotal(t *testing.T) {
	t.Parallel()
	// maxLen must be wide enough to FIT a suffix: at 8 runes (what the cap test
	// next door uses) ReserveForPageSuffix correctly suppresses it, since a
	// one-digit suffix is itself 8 runes. The first version of this test used 8 and
	// failed for that reason — the fixture was wrong, not the reservation.
	const maxLen = 40
	fp := &fakePartialPlatform{failAt: 1000, maxLen: maxLen}
	s := &Scheduler{}
	storeFakeNotifySender(s, map[string]platform.Platform{"fake-notify": fp})

	long := buildDistinctChunks(10, maxLen)
	total := len(platform.SplitText(long, maxLen))
	if total <= cronNotifyMaxChunks {
		t.Fatalf("fixture insufficient: %d chunks, cap is %d", total, cronNotifyMaxChunks)
	}

	s.notifyTarget("fake-notify", "chat-x", long)

	sent := fp.sentChunks()
	if len(sent) == 0 {
		t.Fatal("nothing was sent")
	}
	m := pageSuffixRe.FindStringSubmatch(sent[0])
	if m == nil {
		t.Fatalf("first chunk has no page suffix: %q", sent[0])
	}
	// The delivered count is capped; the reported total must exceed it.
	if m[2] == itoa(len(sent)) && len(sent) < total {
		t.Errorf("reported total %q equals the DELIVERED count %d while %d chunks existed; "+
			"a capped notify must not claim to be complete", m[2], len(sent), total)
	}
}

// TestNotifyTarget_SuffixSuppressedWhenMaxLenTooSmall: a platform whose limit
// cannot fit even the suffix must get bare chunks rather than guaranteed-oversized
// ones (#2057). replyfmt.ReserveForPageSuffix reports that case.
func TestNotifyTarget_SuffixSuppressedWhenMaxLenTooSmall(t *testing.T) {
	t.Parallel()
	// 5 runes is under the 8-rune width of a one-digit suffix.
	const maxLen = 5
	if got, suppress := replyfmt.ReserveForPageSuffix(maxLen, 100); !suppress || got != maxLen {
		t.Fatalf("ReserveForPageSuffix(%d, 100) = (%d, %v), want (%d, true)", maxLen, got, suppress, maxLen)
	}
	fp := &fakePartialPlatform{failAt: 1000, maxLen: maxLen}
	s := &Scheduler{}
	storeFakeNotifySender(s, map[string]platform.Platform{"fake-notify": fp})

	s.notifyTarget("fake-notify", "chat-x", strings.Repeat("abcde", 10))

	for i, chunk := range fp.sentChunks() {
		if pageSuffixRe.MatchString(chunk) {
			t.Errorf("chunk %d carries a suffix at maxLen=%d, which cannot fit one: %q", i+1, maxLen, chunk)
		}
		if n := utf8.RuneCountInString(chunk); n > maxLen {
			t.Errorf("chunk %d is %d runes, over %d", i+1, n, maxLen)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
