package weixin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
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
// resent within one Reply, and the next Reply makes a single fallback try.
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
	if got, want := f.seen(), []string{"t3", "t2", "t1", "t3"}; !slices.Equal(got, want) {
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
