package weixin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
)

// fakeILink is a sendmessage endpoint that records the context_token of every
// request and answers per verdict(token, attempt).
type fakeILink struct {
	mu      sync.Mutex
	tokens  []string
	verdict func(token string, attempt int) string // "ok", "reject", "http500", "drop"
}

// deliverThenDrop wraps singleUse: the first send on token is accepted
// (spending it) and then the connection drops, so the client sees no verdict.
func deliverThenDrop(token string, delivered *atomic.Int32) func(string, int) string {
	base := singleUse()
	var mu sync.Mutex
	dropped := false
	return func(tok string, attempt int) string {
		mu.Lock()
		defer mu.Unlock()
		v := base(tok, attempt)
		if v == "ok" {
			delivered.Add(1)
			if tok == token && !dropped {
				dropped = true
				return "drop"
			}
		}
		return v
	}
}

func (f *fakeILink) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tokens...)
}

func (f *fakeILink) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req sendMessageReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode sendmessage: %v", err)
		}
		f.mu.Lock()
		f.tokens = append(f.tokens, req.Msg.ContextToken)
		attempt := len(f.tokens)
		f.mu.Unlock()
		switch f.verdict(req.Msg.ContextToken, attempt) {
		case "reject":
			_, _ = w.Write([]byte(`{"ret":-14,"errmsg":"context_token used"}`))
		case "http500":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "drop":
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		default:
			_, _ = w.Write([]byte(`{"ret":0}`))
		}
	}
}

// singleUse rejects a token's second use, the iLink semantics behind #3003.
func singleUse() func(string, int) string {
	var mu sync.Mutex
	used := map[string]bool{}
	return func(tok string, _ int) string {
		mu.Lock()
		defer mu.Unlock()
		if used[tok] {
			return "reject"
		}
		used[tok] = true
		return "ok"
	}
}

func newRingTestWeixin(t *testing.T, verdict func(string, int) string, tokens ...string) (*Weixin, *fakeILink) {
	t.Helper()
	f := &fakeILink{verdict: verdict}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	w := New(Config{Token: "tok", BaseURL: srv.URL})
	now := time.Now().UnixNano()
	for _, tok := range tokens {
		w.cacheContextToken("u", tok, now)
	}
	return w, f
}

func reply(w *Weixin) error {
	_, err := w.Reply(context.Background(), platform.OutgoingMessage{ChatID: "u", Text: "answer"})
	return err
}

// TestReply_BackToBackRepliesUseDistinctTokens is the #3003 collect-mode case:
// msg1's answer and msg2's merged answer each need their own token.
func TestReply_BackToBackRepliesUseDistinctTokens(t *testing.T) {
	t.Parallel()
	w, f := newRingTestWeixin(t, singleUse(), "t1", "t2")
	for i := range 2 {
		if err := reply(w); err != nil {
			t.Fatalf("reply %d: %v", i+1, err)
		}
	}
	if got, want := f.seen(), []string{"t2", "t1"}; !slices.Equal(got, want) {
		t.Fatalf("tokens sent = %v, want %v (newest first, each once)", got, want)
	}
}

func TestReply_RejectionFallsThroughToNextToken(t *testing.T) {
	t.Parallel()
	w, f := newRingTestWeixin(t, func(tok string, _ int) string {
		if tok == "t2" {
			return "reject"
		}
		return "ok"
	}, "t1", "t2")
	if err := reply(w); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if got, want := f.seen(), []string{"t2", "t1"}; !slices.Equal(got, want) {
		t.Fatalf("tokens sent = %v, want %v", got, want)
	}
}

// TestReply_NonRejectionErrorDoesNotRotate: an error without a parsed verdict
// may follow a delivery, so Reply must not resend on another token, and the
// token goes back to the pool for ReplyWithRetry's next attempt.
func TestReply_NonRejectionErrorDoesNotRotate(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"http500", "drop"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			w, f := newRingTestWeixin(t, func(_ string, attempt int) string {
				if attempt == 1 {
					return mode
				}
				return "ok"
			}, "t1", "t2")
			err := reply(w)
			if err == nil {
				t.Fatal("reply succeeded, want the first attempt's error")
			}
			if errors.Is(err, errUpstreamRejected) {
				t.Fatalf("err = %v, must not be classed as an upstream rejection", err)
			}
			if got, want := f.seen(), []string{"t2"}; !slices.Equal(got, want) {
				t.Fatalf("tokens sent = %v, want %v (no rotation)", got, want)
			}
			if err := reply(w); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if got, want := f.seen(), []string{"t2", "t2"}; !slices.Equal(got, want) {
				t.Fatalf("tokens sent = %v, want %v (retry reuses the released token)", got, want)
			}
		})
	}
}

// TestReply_AllSpentFallsBackToNewest keeps the pre-ring behaviour as the last
// resort: with nothing unspent, Reply still tries the newest token once.
func TestReply_AllSpentFallsBackToNewest(t *testing.T) {
	t.Parallel()
	w, f := newRingTestWeixin(t, func(string, int) string { return "ok" }, "t1", "t2")
	for i := range 3 {
		if err := reply(w); err != nil {
			t.Fatalf("reply %d: %v", i+1, err)
		}
	}
	if got, want := f.seen(), []string{"t2", "t1", "t2"}; !slices.Equal(got, want) {
		t.Fatalf("tokens sent = %v, want %v", got, want)
	}
}

// TestReply_AllRejectedTriesEachTokenOnce bounds the rotation: no token is
// resent within one Reply, and since upstream consumed none of them, the
// next Reply may try them all again.
func TestReply_AllRejectedTriesEachTokenOnce(t *testing.T) {
	t.Parallel()
	w, f := newRingTestWeixin(t, func(string, int) string { return "reject" }, "t1", "t2", "t3")
	err := reply(w)
	if !errors.Is(err, errUpstreamRejected) {
		t.Fatalf("err = %v, want an upstream rejection", err)
	}
	if got, want := f.seen(), []string{"t3", "t2", "t1"}; !slices.Equal(got, want) {
		t.Fatalf("tokens sent = %v, want %v", got, want)
	}
	if err := reply(w); !errors.Is(err, errUpstreamRejected) {
		t.Fatalf("second reply err = %v, want an upstream rejection", err)
	}
	if got, want := f.seen(), []string{"t3", "t2", "t1", "t3", "t2", "t1"}; !slices.Equal(got, want) {
		t.Fatalf("tokens sent = %v, want %v", got, want)
	}
}

// TestReply_RejectedMessageDoesNotBurnTokens: a rejection that has nothing to
// do with the token (content, rate limit) must leave the tokens for the
// replies that follow.
func TestReply_RejectedMessageDoesNotBurnTokens(t *testing.T) {
	t.Parallel()
	base := singleUse()
	w, f := newRingTestWeixin(t, func(tok string, attempt int) string {
		if attempt <= 2 {
			return "reject"
		}
		return base(tok, attempt)
	}, "t1", "t2")
	if err := reply(w); !errors.Is(err, errUpstreamRejected) {
		t.Fatalf("err = %v, want an upstream rejection", err)
	}
	for i := range 2 {
		if err := reply(w); err != nil {
			t.Fatalf("reply %d after the rejection: %v", i+1, err)
		}
	}
	if got, want := f.seen(), []string{"t2", "t1", "t2", "t1"}; !slices.Equal(got, want) {
		t.Fatalf("tokens sent = %v, want %v", got, want)
	}
}

// TestReplyWithRetry_DeliveredThenDroppedSendsOnce: the send lands but the
// client gets no verdict. The retry's rejection on that token must end the
// retries, not move the message onto another token.
func TestReplyWithRetry_DeliveredThenDroppedSendsOnce(t *testing.T) {
	t.Parallel()
	var delivered atomic.Int32
	w, f := newRingTestWeixin(t, deliverThenDrop("t2", &delivered), "t1", "t2")
	_, err := platform.ReplyWithRetry(context.Background(), w,
		platform.OutgoingMessage{ChatID: "u", Text: "answer"}, 3)
	if !platform.IsPermanent(err) || !errors.Is(err, errUpstreamRejected) {
		t.Fatalf("err = %v, want a permanent upstream rejection", err)
	}
	if n := delivered.Load(); n != 1 {
		t.Fatalf("delivered %d times, want 1", n)
	}
	if got, want := f.seen(), []string{"t2", "t2"}; !slices.Equal(got, want) {
		t.Fatalf("tokens sent = %v, want %v", got, want)
	}
	if err := reply(w); err != nil {
		t.Fatalf("next reply: %v (t1 must still be unspent)", err)
	}
}

// TestReply_UncertainTokenIsRetriedBeforeANewerOne: a token that arrives
// while the retry backs off must not carry a possibly delivered message
// a second time.
func TestReply_UncertainTokenIsRetriedBeforeANewerOne(t *testing.T) {
	t.Parallel()
	var delivered atomic.Int32
	w, f := newRingTestWeixin(t, deliverThenDrop("t2", &delivered), "t1", "t2")
	if err := reply(w); err == nil {
		t.Fatal("reply succeeded, want the dropped connection's error")
	}
	w.cacheContextToken("u", "t3", time.Now().UnixNano())
	if err := reply(w); !platform.IsPermanent(err) {
		t.Fatalf("retry err = %v, want permanent", err)
	}
	if n := delivered.Load(); n != 1 {
		t.Fatalf("delivered %d times, want 1 (tokens sent %v)", n, f.seen())
	}
	if err := reply(w); err != nil {
		t.Fatalf("next reply: %v (t3 must still be unspent)", err)
	}
	if got, want := f.seen(), []string{"t2", "t2", "t3"}; !slices.Equal(got, want) {
		t.Fatalf("tokens sent = %v, want %v", got, want)
	}
}

func TestReply_ConcurrentRepliesTakeDistinctTokens(t *testing.T) {
	t.Parallel()
	w, f := newRingTestWeixin(t, singleUse(), "t1", "t2")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Go(func() { errs[i] = reply(w) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("reply %d: %v", i+1, err)
		}
	}
	got := f.seen()
	slices.Sort(got)
	if want := []string{"t1", "t2"}; !slices.Equal(got, want) {
		t.Fatalf("tokens sent = %v, want %v", got, want)
	}
}

func ringTokens(t *testing.T, w *Weixin, user string) []string {
	t.Helper()
	v, ok := w.contextTokens.Load(user)
	if !ok {
		return nil
	}
	r := v.(*tokenRing)
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.slots))
	for _, s := range r.slots {
		out = append(out, s.token)
	}
	return out
}

func TestCacheContextToken_CapEvictsOldestAndDedups(t *testing.T) {
	t.Parallel()
	w := New(Config{Token: "tok"})
	for _, tok := range []string{"t1", "t2", "t3", "t3", "t4", "t5"} {
		w.cacheContextToken("u", tok, 1)
	}
	if got, want := ringTokens(t, w, "u"), []string{"t2", "t3", "t4", "t5"}; !slices.Equal(got, want) {
		t.Fatalf("ring = %v, want %v", got, want)
	}
}

// TestCacheContextToken_RedeliveryKeepsSpentState: a relay redelivering an
// already-answered message must not hand its spent token out as fresh.
func TestCacheContextToken_RedeliveryKeepsSpentState(t *testing.T) {
	t.Parallel()
	w, f := newRingTestWeixin(t, singleUse(), "t1", "t2")
	if err := reply(w); err != nil {
		t.Fatalf("reply: %v", err)
	}
	w.cacheContextToken("u", "t2", time.Now().UnixNano())
	if err := reply(w); err != nil {
		t.Fatalf("reply after redelivery: %v", err)
	}
	if got, want := f.seen(), []string{"t2", "t1"}; !slices.Equal(got, want) {
		t.Fatalf("tokens sent = %v, want %v", got, want)
	}
}

func TestEvictIdleTokens_UsesNewestStamp(t *testing.T) {
	t.Parallel()
	w := New(Config{Token: "tok"})
	w.cacheContextToken("idle", "a1", 100)
	w.cacheContextToken("active", "b1", 100)
	w.cacheContextToken("active", "b2", 300)

	w.evictIdleTokens(200)

	if _, ok := w.contextTokens.Load("idle"); ok {
		t.Error("idle user's ring survived eviction")
	}
	if got, want := ringTokens(t, w, "active"), []string{"b1", "b2"}; !slices.Equal(got, want) {
		t.Errorf("active ring = %v, want %v (a fresh push keeps the whole ring)", got, want)
	}
}

// TestCacheContextToken_EvictedRingIsReplaced covers the push racing the
// sweep: a ring marked evicted must not swallow the token.
func TestCacheContextToken_EvictedRingIsReplaced(t *testing.T) {
	t.Parallel()
	w := New(Config{Token: "tok"})
	w.cacheContextToken("u", "old", 100)
	v, _ := w.contextTokens.Load("u")
	stale := v.(*tokenRing)
	if !stale.evictIfIdle(200) {
		t.Fatal("evictIfIdle(200) = false for a ring stamped 100")
	}
	// The sweep marked the ring but has not deleted it yet.
	w.cacheContextToken("u", "new", 300)
	if got, want := ringTokens(t, w, "u"), []string{"new"}; !slices.Equal(got, want) {
		t.Fatalf("ring = %v, want %v", got, want)
	}
}

// TestEvictIdleTokens_SweepKeepsAFreshRing covers the sweep side of the same
// race: its delete must not drop the ring a push stored after the eviction.
func TestEvictIdleTokens_SweepKeepsAFreshRing(t *testing.T) {
	t.Parallel()
	w := New(Config{Token: "tok"})
	w.cacheContextToken("u", "old", 100)
	v, _ := w.contextTokens.Load("u")
	stale := v.(*tokenRing)
	stale.evictIfIdle(200)
	w.cacheContextToken("u", "new", 300)

	w.dropEvictedRing("u", stale)

	if got, want := ringTokens(t, w, "u"), []string{"new"}; !slices.Equal(got, want) {
		t.Fatalf("ring = %v, want %v", got, want)
	}
}

func replyText(w *Weixin, text string) error {
	_, err := w.Reply(context.Background(), platform.OutgoingMessage{ChatID: "u", Text: text})
	return err
}

// TestReplyWithRetry_OtherMessageSkipsUncertainToken: a token left uncertain
// by one message's no-verdict send must not turn the next message's
// rejection into a give-up; the next message goes out on a fresh token.
func TestReplyWithRetry_OtherMessageSkipsUncertainToken(t *testing.T) {
	t.Parallel()
	var delivered atomic.Int32
	w, f := newRingTestWeixin(t, deliverThenDrop("t1", &delivered), "t1")
	if err := replyText(w, "help text"); err == nil {
		t.Fatal("reply succeeded, want the dropped connection's error")
	}
	w.cacheContextToken("u", "t2", time.Now().UnixNano())
	if _, err := platform.ReplyWithRetry(context.Background(), w,
		platform.OutgoingMessage{ChatID: "u", Text: "answer to msg2"}, 3); err != nil {
		t.Fatalf("answer: %v (tokens sent %v)", err, f.seen())
	}
	if got, want := f.seen(), []string{"t1", "t2"}; !slices.Equal(got, want) {
		t.Fatalf("tokens sent = %v, want %v", got, want)
	}
}

// ringState renders each slot as token, token+"!" (spent), token+"?"
// (uncertain) or token+"!?" (spent, its uncertain message most likely landed).
func ringState(t *testing.T, w *Weixin) []string {
	t.Helper()
	v, _ := w.contextTokens.Load("u")
	r := v.(*tokenRing)
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.slots))
	for _, s := range r.slots {
		switch {
		case s.spent && s.uncertain:
			out = append(out, s.token+"!?")
		case s.spent:
			out = append(out, s.token+"!")
		case s.uncertain:
			out = append(out, s.token+"?")
		default:
			out = append(out, s.token)
		}
	}
	return out
}

// TestReply_OtherMessageRejectedOnUncertainTokenRotates: with no fresh token
// left, another message's rejection on the uncertain token is an ordinary
// rejection (retryable, not "maybe delivered"), and the token stays spent.
func TestReply_OtherMessageRejectedOnUncertainTokenRotates(t *testing.T) {
	t.Parallel()
	var delivered atomic.Int32
	w, f := newRingTestWeixin(t, deliverThenDrop("t1", &delivered), "t1")
	if err := replyText(w, "help text"); err == nil {
		t.Fatal("reply succeeded, want the dropped connection's error")
	}
	err := replyText(w, "answer")
	if !errors.Is(err, errUpstreamRejected) || platform.IsPermanent(err) {
		t.Fatalf("err = %v, want a retryable upstream rejection", err)
	}
	if got, want := ringState(t, w), []string{"t1!?"}; !slices.Equal(got, want) {
		t.Fatalf("ring = %v, want %v (tokens sent %v)", got, want, f.seen())
	}
}

// TestReply_OtherMessageUsesUncertainTokenLast: an uncertain token is still a
// candidate for another message once the fresh ones are gone, since the
// no-verdict send may not have landed.
func TestReply_OtherMessageUsesUncertainTokenLast(t *testing.T) {
	t.Parallel()
	w, f := newRingTestWeixin(t, func(_ string, attempt int) string {
		if attempt == 1 {
			return "http500"
		}
		return "ok"
	}, "t1", "t2")
	if err := replyText(w, "help text"); err == nil {
		t.Fatal("reply succeeded, want the http 500")
	}
	for _, text := range []string{"answer 1", "answer 2"} {
		if err := replyText(w, text); err != nil {
			t.Fatalf("%s: %v", text, err)
		}
	}
	if got, want := f.seen(), []string{"t2", "t1", "t2"}; !slices.Equal(got, want) {
		t.Fatalf("tokens sent = %v, want %v", got, want)
	}
}

// TestReply_RejectionStateAfterReply: a rejected token stays spent only when
// a later token in the same Reply is accepted.
func TestReply_RejectionStateAfterReply(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, second string
		wantErr      bool
		want         []string
	}{
		{"then accepted", "ok", false, []string{"t1!", "t2!"}},
		{"then no verdict", "http500", true, []string{"t1?", "t2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, _ := newRingTestWeixin(t, func(tok string, _ int) string {
				if tok == "t2" {
					return "reject"
				}
				return tc.second
			}, "t1", "t2")
			if err := reply(w); (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got := ringState(t, w); !slices.Equal(got, tc.want) {
				t.Fatalf("ring = %v, want %v", got, tc.want)
			}
		})
	}
}

// ageUncertain backdates every slot's uncertain stamp by d.
func ageUncertain(t *testing.T, w *Weixin, d time.Duration) {
	t.Helper()
	v, _ := w.contextTokens.Load("u")
	r := v.(*tokenRing)
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.slots {
		r.slots[i].uncertainNs -= int64(d)
	}
}

// TestReply_StaleUncertainTokenDoesNotBindEqualText: once the affinity window
// has passed, a fixed text sent again answers a different user message and
// goes out on a fresh token, not onto the old uncertain one.
func TestReply_StaleUncertainTokenDoesNotBindEqualText(t *testing.T) {
	t.Parallel()
	const text = "处理出错，请稍后重试"
	var delivered atomic.Int32
	w, f := newRingTestWeixin(t, deliverThenDrop("t1", &delivered), "t1")
	if err := replyText(w, text); err == nil {
		t.Fatal("reply succeeded, want the dropped connection's error")
	}
	ageUncertain(t, w, retryAffinityWindow+time.Second)
	w.cacheContextToken("u", "t2", time.Now().UnixNano())
	if err := replyText(w, text); err != nil {
		t.Fatalf("later reply: %v (tokens sent %v)", err, f.seen())
	}
	if got, want := f.seen(), []string{"t1", "t2"}; !slices.Equal(got, want) {
		t.Fatalf("tokens sent = %v, want %v", got, want)
	}
}

// TestReply_StoppedRetryFreesItsText: a retry that stopped on its uncertain
// token has concluded; an equal text after it is not stopped as well.
func TestReply_StoppedRetryFreesItsText(t *testing.T) {
	t.Parallel()
	var delivered atomic.Int32
	w, f := newRingTestWeixin(t, deliverThenDrop("t1", &delivered), "t1")
	if err := reply(w); err == nil {
		t.Fatal("reply succeeded, want the dropped connection's error")
	}
	if err := reply(w); !platform.IsPermanent(err) {
		t.Fatalf("retry err = %v, want permanent", err)
	}
	w.cacheContextToken("u", "t2", time.Now().UnixNano())
	if err := reply(w); err != nil {
		t.Fatalf("equal text after the stop: %v (tokens sent %v)", err, f.seen())
	}
	if got, want := f.seen(), []string{"t1", "t1", "t2"}; !slices.Equal(got, want) {
		t.Fatalf("tokens sent = %v, want %v", got, want)
	}
}

// TestReply_RetryStopsWhenAnotherMessageWasRejectedOnItsToken: B's rejection
// on A's uncertain token says A's send landed. A's retry must stop without
// sending, even with a fresh token pushed meanwhile. The window counts from
// A's no-verdict send; past it the inference is dropped.
func TestReply_RetryStopsWhenAnotherMessageWasRejectedOnItsToken(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		age      time.Duration
		wantStop bool
	}{
		{"within window", 0, true},
		{"past window", retryAffinityWindow + time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var delivered atomic.Int32
			w, f := newRingTestWeixin(t, deliverThenDrop("t2", &delivered), "t2")
			if err := replyText(w, "A"); err == nil {
				t.Fatal("A succeeded, want the dropped connection's error")
			}
			ageUncertain(t, w, tc.age)
			if err := replyText(w, "B"); !errors.Is(err, errUpstreamRejected) || platform.IsPermanent(err) {
				t.Fatalf("B err = %v, want a retryable upstream rejection", err)
			}
			w.cacheContextToken("u", "t3", time.Now().UnixNano())
			err := replyText(w, "A")
			if got := platform.IsPermanent(err); got != tc.wantStop || (!tc.wantStop && err != nil) {
				t.Fatalf("A retry err = %v, want stop %v (tokens sent %v)", err, tc.wantStop, f.seen())
			}
			if tc.wantStop {
				// The stop concludes A: an equal text after it is sent.
				if err := replyText(w, "A"); err != nil {
					t.Fatalf("equal text after the stop: %v (tokens sent %v)", err, f.seen())
				}
			}
			// Either way one A goes out on t3; the stopped retry sends nothing.
			if got, want := f.seen(), []string{"t2", "t2", "t3"}; !slices.Equal(got, want) {
				t.Fatalf("tokens sent = %v, want %v", got, want)
			}
		})
	}
}
