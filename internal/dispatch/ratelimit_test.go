package dispatch

import (
	"context"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/imauth"
	"github.com/naozhi/naozhi/internal/platform"
)

const rateLimitedReply = "消息过于频繁"

func newRateLimitDispatcher(t *testing.T, rl RateLimit) (*Dispatcher, *fakePlatform, *countingTurns) {
	t.Helper()
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, func(cfg *testDispatcherConfig) { cfg.RateLimit = rl })
	ct := &countingTurns{Turns: d.turns}
	d.turns = ct
	return d, fp, ct
}

var rateEvent atomic.Int64

func rateMsg(user, chatID, text string) platform.IncomingMessage {
	return platform.IncomingMessage{
		Platform: "fake", EventID: "rate-" + strconv.FormatInt(rateEvent.Add(1), 10),
		UserID: user, ChatID: chatID, ChatType: "direct", Text: text,
	}
}

// accepted sends msgs through prepareInbound and reports which got through.
func accepted(d *Dispatcher, msgs ...platform.IncomingMessage) []bool {
	out := make([]bool, len(msgs))
	for i, m := range msgs {
		_, out[i] = d.prepareInbound(context.Background(), m)
	}
	return out
}

func countReplies(fp *fakePlatform, substr string) int {
	n := 0
	for _, r := range fp.allReplies() {
		if strings.Contains(r, substr) {
			n++
		}
	}
	return n
}

// A sender past the burst is dropped and told once; another sender keeps a
// full bucket of its own.
func TestRateLimit_BurstThenSilentDrops(t *testing.T) {
	d, fp, _ := newRateLimitDispatcher(t, RateLimit{MsgsPerMin: 1, Burst: 2})
	before := dispatchRateLimitedTotal.Value()

	got := accepted(d,
		rateMsg("alice", "c1", "one"), rateMsg("alice", "c1", "two"),
		rateMsg("alice", "c1", "three"), rateMsg("alice", "c2", "four"))
	if want := []bool{true, true, false, false}; !slices.Equal(got, want) {
		t.Fatalf("alice accepted = %v, want %v (the bucket is per sender, not per chat)", got, want)
	}
	if n := countReplies(fp, rateLimitedReply); n != 1 {
		t.Errorf("rate-limit replies = %d, want 1: %q", n, fp.allReplies())
	}
	if moved := dispatchRateLimitedTotal.Value() - before; moved != 2 {
		t.Errorf("naozhi_dispatch_rate_limited_total moved by %d, want 2", moved)
	}
	if got := accepted(d, rateMsg("bob", "c1", "hi"), rateMsg("bob", "c1", "hi")); !slices.Equal(got, []bool{true, true}) {
		t.Errorf("bob accepted = %v, alice's bucket leaked into his", got)
	}
}

// Slash commands draw from the bucket (command spam is spam), except /stop,
// which only ever ends spend. The exemption reads /stop the way
// dispatchCommand does, so IME capitalisation counts.
func TestRateLimit_CommandsLimitedStopExempt(t *testing.T) {
	d, fp, ct := newRateLimitDispatcher(t, RateLimit{MsgsPerMin: 1, Burst: 1})
	accepted(d, rateMsg("alice", "c1", "/help"), rateMsg("alice", "c1", "/new"))
	if n := ct.resets.Load(); n != 0 {
		t.Errorf("/new past the limit reset the session %d times", n)
	}
	for _, stop := range []string{"/stop", "/Stop", "/STOP now", "/Stop\u3000"} {
		before := countReplies(fp, "当前没有正在进行的回复")
		accepted(d, rateMsg("alice", "c1", stop))
		if countReplies(fp, "当前没有正在进行的回复") == before {
			t.Errorf("%q past the limit was not handled: replies %q", stop, fp.allReplies())
		}
	}
}

// A message with no sender ID buckets by its chat, never under the empty key
// (which the limiter refuses outright).
func TestRateLimit_EmptyUserBucketsByChat(t *testing.T) {
	d, _, _ := newRateLimitDispatcher(t, RateLimit{MsgsPerMin: 1, Burst: 1})
	got := accepted(d, rateMsg("", "c1", "a"), rateMsg("", "c1", "b"), rateMsg("", "c2", "c"))
	if want := []bool{true, false, true}; !slices.Equal(got, want) {
		t.Errorf("accepted = %v, want %v", got, want)
	}
}

// Burst 0 defaults to msgs_per_min.
func TestRateLimit_BurstDefaultsToRate(t *testing.T) {
	d, _, _ := newRateLimitDispatcher(t, RateLimit{MsgsPerMin: 3})
	got := accepted(d, rateMsg("u", "c", "1"), rateMsg("u", "c", "2"), rateMsg("u", "c", "3"), rateMsg("u", "c", "4"))
	if want := []bool{true, true, true, false}; !slices.Equal(got, want) {
		t.Errorf("accepted = %v, want %v", got, want)
	}
}

// The zero value is unlimited.
func TestRateLimit_ZeroIsUnlimited(t *testing.T) {
	d, fp, _ := newRateLimitDispatcher(t, RateLimit{})
	for i := range 50 {
		if got := accepted(d, rateMsg("u", "c", "m"+strconv.Itoa(i))); !got[0] {
			t.Fatalf("message %d refused with no limit configured", i)
		}
	}
	if n := countReplies(fp, rateLimitedReply); n != 0 {
		t.Errorf("rate-limit replies = %d with no limit", n)
	}
}

// Un-mentioned group chatter is dropped before the limiter, so it spends no
// tokens of the sender who later @-mentions the bot.
func TestRateLimit_UnmentionedGroupSpendsNothing(t *testing.T) {
	d, _, _ := newRateLimitDispatcher(t, RateLimit{MsgsPerMin: 1, Burst: 1})
	for range 5 {
		m := rateMsg("alice", "g1", "chatter")
		m.ChatType = "group"
		accepted(d, m)
	}
	m := rateMsg("alice", "g1", "@bot hi")
	m.ChatType, m.MentionMe = "group", true
	if got := accepted(d, m); !got[0] {
		t.Error("first mention refused: un-mentioned messages consumed the bucket")
	}
}

// A sender the access policy refuses spends no tokens: authorization runs
// first.
func TestRateLimit_DeniedSenderSpendsNothing(t *testing.T) {
	d, _, _ := newRateLimitDispatcher(t, RateLimit{MsgsPerMin: 1, Burst: 1})
	d.SetAccessPolicy(&imauth.Policy{DefaultDeny: true})
	for range 3 {
		accepted(d, rateMsg("alice", "c1", "hi"))
	}
	d.SetAccessPolicy(nil)
	if got := accepted(d, rateMsg("alice", "c1", "hi")); !got[0] {
		t.Error("first allowed message refused: denied messages consumed the bucket")
	}
}

// The "too fast" reply is throttled per bucket for rateLimitReplyWindow.
func TestRateLimit_ReplyWindow(t *testing.T) {
	th := denyThrottle{window: rateLimitReplyWindow}
	t0 := time.Unix(1_700_000_000, 0)
	if !th.allow("k", t0) || th.allow("k", t0.Add(rateLimitReplyWindow-time.Second)) {
		t.Fatal("second reply inside the window answered, or the first was not")
	}
	if !th.allow("k", t0.Add(rateLimitReplyWindow)) {
		t.Error("reply after the window not answered")
	}
}

// A bucket is reset no sooner than it would refill, so a large burst cannot
// be regained by idling for the floor TTL.
func TestInboundLimitConfig_TTLCoversRefill(t *testing.T) {
	for _, tc := range []struct {
		rl   RateLimit
		want time.Duration
	}{
		{RateLimit{MsgsPerMin: 1}, rateLimitIdleTTL},
		{RateLimit{MsgsPerMin: 6, Burst: 60}, rateLimitIdleTTL},
		{RateLimit{MsgsPerMin: 1, Burst: 30}, 30 * time.Minute},
		{RateLimit{MsgsPerMin: 2, Burst: 45}, 22*time.Minute + 30*time.Second},
		{RateLimit{MsgsPerMin: 1, Burst: math.MaxInt}, math.MaxInt64},
	} {
		if got := inboundLimitConfig(tc.rl).TTL; got != tc.want {
			t.Errorf("inboundLimitConfig(%+v).TTL = %v, want %v", tc.rl, got, tc.want)
		}
	}
}
