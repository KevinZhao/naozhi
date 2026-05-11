package node

import (
	"os"
	"regexp"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

// R184-REL-M2 — Subscribe must seed r.lastEvent[key] with the caller's
// `after` timestamp atomically with the r.subs[key] write, so a racing
// reconnect() taking a snapshot moments later never sees
// lastEvent[key]=0 for a key whose subscribers already asked for
// after=N. Without the seed, the reconnect path resends
// subscribe(key, after=0), triggering a full server-side history
// replay that duplicates events on the browser.

// seedTestSink is a noop EventSink used purely to occupy r.subs without
// exercising the network path — the test asserts on relay-internal state
// after Subscribe returns.
type seedTestSink struct{ _ atomic.Int32 }

func (s *seedTestSink) SendJSON(_ any)   {}
func (s *seedTestSink) SendRaw(_ []byte) {}

// TestWSRelay_Subscribe_SeedsLastEventForRaceSafeReconnect pins the
// R184-REL-M2 invariant directly. We bypass ensureConnected() by
// pre-populating r.conn with a dummy websocket so Subscribe's internal
// bookkeeping runs without a real server roundtrip. After Subscribe
// returns, a reconnect() lookalike snapshot MUST observe the seeded
// lastEvent value, not zero.
func TestWSRelay_Subscribe_SeedsLastEventForRaceSafeReconnect(t *testing.T) {
	t.Parallel()

	srv := wsTestServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		if !authHandshake(t, conn) {
			return
		}
		// Drain one subscribe frame then hang so the test controls the
		// lifecycle.
		var msg ClientMsg
		_ = conn.ReadJSON(&msg)
		// Block until the connection is closed by the client side.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer srv.Close()

	node := newRelayNode(srv)
	r := newWSRelay(node)
	defer r.Close()

	const key = "feishu:direct:u1:general"
	const after int64 = 1_700_000_000_000 // arbitrary but non-zero unix ms

	sink := &seedTestSink{}
	r.Subscribe(sink, key, after)

	// Snapshot the same way reconnect() does. Without the seed,
	// lastEvent[key] is the zero value because no event has flowed from
	// the server yet — and reconnect() would re-emit subscribe(after=0)
	// for this key, causing server-side full history replay.
	r.mu.Lock()
	got := r.lastEvent[key]
	_, hasKey := r.subs[key]
	r.mu.Unlock()

	if !hasKey {
		t.Fatalf("subscribe did not register sink in r.subs[%q]", key)
	}
	if got != after {
		t.Errorf("lastEvent[%q] = %d, want %d — R184-REL-M2 seed missing; "+
			"a racing reconnect would resend subscribe(after=0) and "+
			"trigger server-side full history replay",
			key, got, after)
	}
}

// TestWSRelay_Subscribe_SeedOnlyOnFirstSubscriber verifies the seed
// only fires for the first subscriber of a key. Subsequent subscribers
// route through sendHistoryToClient (HTTP fetch for just that sink) and
// must NOT overwrite lastEvent[key] with their own `after` — doing so
// would let a late joiner with a small after value regress the
// reconnect-snapshot baseline and again trigger excess replay.
func TestWSRelay_Subscribe_SeedOnlyOnFirstSubscriber(t *testing.T) {
	t.Parallel()

	srv := wsTestServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		if !authHandshake(t, conn) {
			return
		}
		var msg ClientMsg
		_ = conn.ReadJSON(&msg)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer srv.Close()

	node := newRelayNode(srv)
	r := newWSRelay(node)
	defer r.Close()

	const key = "feishu:direct:u1:general"
	const firstAfter int64 = 1_700_000_000_000
	const laterAfter int64 = 1 // would REGRESS lastEvent if (wrongly) written

	r.Subscribe(&seedTestSink{}, key, firstAfter)
	r.Subscribe(&seedTestSink{}, key, laterAfter)

	r.mu.Lock()
	got := r.lastEvent[key]
	r.mu.Unlock()

	if got != firstAfter {
		t.Errorf("lastEvent[%q] = %d, want %d — second subscriber must "+
			"not overwrite the seed with its own (possibly smaller) after",
			key, got, firstAfter)
	}
}

// TestWSRelay_Source_SubscribeSeedsLastEvent is a source-level anchor
// that the seed write sits inside Subscribe's r.mu critical section,
// guarded against the first-subscriber branch. A future refactor that
// hoists the seed outside r.mu or drops it entirely reopens R184-REL-M2
// without failing the behavioural test if the goroutine happens to
// schedule favourably; this test fails fast at build time.
func TestWSRelay_Source_SubscribeSeedsLastEvent(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("relay.go")
	if err != nil {
		t.Fatalf("read relay.go: %v", err)
	}
	src := string(data)

	// Anchor: Subscribe() must seed r.lastEvent[key] with `after` inside
	// the same r.mu.Lock/Unlock window as the r.subs[key] append, and
	// the write must be gated on `!alreadySubscribed` so late joiners do
	// not clobber the seed. The pattern accepts either the seed line
	// sitting before or after the subs append, so long as both are in
	// the lock and the seed is guarded.
	pat := regexp.MustCompile(`(?s)func \(r \*wsRelay\) Subscribe\([^)]*\) \{.*?` +
		`r\.mu\.Lock\(\).*?` +
		`if !alreadySubscribed \{[^}]*r\.lastEvent\[key\] = after[^}]*\}.*?` +
		`r\.mu\.Unlock\(\)`)
	if !pat.MatchString(src) {
		t.Error("Subscribe() must seed r.lastEvent[key] = after inside the " +
			"r.mu critical section guarded by !alreadySubscribed (R184-REL-M2). " +
			"Without the seed, reconnect()'s resubscribe snapshot sees " +
			"lastEvent[key]=0 for a key added between connect() and the " +
			"lock acquisition, and the server replays full history.")
	}
}
