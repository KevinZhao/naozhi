package session

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/node"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
)

// noNodeAccessor is a single-node NodeAccessor stub: it reports zero known
// nodes so HandleList takes the buildLocalResp (single-node) path where the
// R20260607-PERF-1 (#1916) storeGen-gated ETag fast path is active.
type noNodeAccessor struct{}

func (noNodeAccessor) HasNodes() bool                                           { return false }
func (noNodeAccessor) NodesSnapshot() map[string]node.Conn                      { return nil }
func (noNodeAccessor) NodeByID(string) (node.Conn, bool)                        { return nil, false }
func (noNodeAccessor) LookupNode(http.ResponseWriter, string) (node.Conn, bool) { return nil, false }
func (noNodeAccessor) KnownNodes() map[string]string                            { return nil }

// multiNodeAccessor reports a known node so HandleList takes the multi-node
// path, where the conditional 304 fast path is intentionally disabled.
type multiNodeAccessor struct{ noNodeAccessor }

func (multiNodeAccessor) KnownNodes() map[string]string { return map[string]string{"peer": "Peer"} }
func (multiNodeAccessor) HasNodes() bool                { return false }

func newIfChangedTestHandlers(t *testing.T, na NodeAccessor) *Handlers {
	t.Helper()
	r := sessionpkg.NewRouter(sessionpkg.RouterConfig{MaxProcs: 3})
	return New(Deps{
		Router:        r,
		NodeAccess:    na,
		NodeCache:     node.NewCacheManager(func() map[string]node.Conn { return nil }, func() {}),
		StartedAt:     time.Now(),
		WatchdogNoOut: &atomic.Int64{},
		WatchdogTotal: &atomic.Int64{},
		// ClaudeDir intentionally empty: historySessions() short-circuits to
		// nil with no filesystem scan, keeping the test hermetic.
	})
}

func doList(h *Handlers, ifNoneMatch string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	h.HandleList(rec, req)
	return rec
}

// TestHandleList_UnchangedReturns304 verifies the "未变化时不重建快照" path:
// a second poll echoing the ETag from the first poll, with no storeGen
// advance in between, returns 304 with an empty body and the same ETag.
func TestHandleList_UnchangedReturns304(t *testing.T) {
	h := newIfChangedTestHandlers(t, noNodeAccessor{})

	// First poll: no validator → full 200 carrying an ETag.
	rec1 := doList(h, "")
	if rec1.Code != http.StatusOK {
		t.Fatalf("first poll status = %d, want 200", rec1.Code)
	}
	etag := rec1.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("first poll did not set an ETag header")
	}
	if rec1.Body.Len() == 0 {
		t.Fatalf("first poll body is empty, want full JSON")
	}

	// Second poll echoing the ETag, nothing changed → 304, empty body.
	rec2 := doList(h, etag)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("unchanged poll status = %d, want 304", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Fatalf("304 body len = %d, want 0 (no snapshot rebuild / marshal)", rec2.Body.Len())
	}
	if got := rec2.Header().Get("ETag"); got != etag {
		t.Fatalf("304 ETag = %q, want unchanged %q", got, etag)
	}
}

// TestHandleList_ChangedRebuilds verifies the "变化时重建" path: after the
// session storeGen advances, a poll carrying the stale ETag must rebuild
// (200) and hand back a fresh, different ETag.
func TestHandleList_ChangedRebuilds(t *testing.T) {
	h := newIfChangedTestHandlers(t, noNodeAccessor{})

	rec1 := doList(h, "")
	etag1 := rec1.Header().Get("ETag")
	if etag1 == "" {
		t.Fatalf("first poll did not set an ETag header")
	}

	// Advance storeGen the same way a render-affecting mutation would.
	h.router.BumpVersion()

	rec2 := doList(h, etag1)
	if rec2.Code != http.StatusOK {
		t.Fatalf("changed poll status = %d, want 200 (rebuild)", rec2.Code)
	}
	if rec2.Body.Len() == 0 {
		t.Fatalf("changed poll body is empty, want full JSON")
	}
	etag2 := rec2.Header().Get("ETag")
	if etag2 == "" {
		t.Fatalf("changed poll did not set an ETag header")
	}
	if etag2 == etag1 {
		t.Fatalf("ETag did not change after BumpVersion: %q", etag2)
	}

	// The fresh ETag must now itself drive a 304 on the next unchanged poll.
	rec3 := doList(h, etag2)
	if rec3.Code != http.StatusNotModified {
		t.Fatalf("re-stabilised poll status = %d, want 304", rec3.Code)
	}
}

// TestHandleList_FirstPollNoValidator confirms a client that never sends
// If-None-Match always receives a full 200 — the optimisation is opt-in and
// backward compatible.
func TestHandleList_FirstPollNoValidator(t *testing.T) {
	h := newIfChangedTestHandlers(t, noNodeAccessor{})
	rec := doList(h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("no-validator poll status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatalf("no-validator poll must still advertise an ETag")
	}
}

// TestHandleList_StaleETagRebuilds confirms a non-matching (e.g. from an
// older binary / different content) validator never yields a spurious 304.
func TestHandleList_StaleETagRebuilds(t *testing.T) {
	h := newIfChangedTestHandlers(t, noNodeAccessor{})
	rec := doList(h, `"v999999-h0-n0"`)
	if rec.Code != http.StatusOK {
		t.Fatalf("stale-ETag poll status = %d, want 200 (no spurious 304)", rec.Code)
	}
}

// TestHandleList_MultiNodeNeverShortCircuits confirms the conditional 304 is
// scoped to single-node deployments: with a known node the handler always
// rebuilds, even when the client echoes whatever ETag it last saw, because
// the multi-node body folds in live node status with no version hook.
func TestHandleList_MultiNodeNeverShortCircuits(t *testing.T) {
	h := newIfChangedTestHandlers(t, multiNodeAccessor{})

	rec1 := doList(h, "")
	if rec1.Code != http.StatusOK {
		t.Fatalf("multi-node first poll status = %d, want 200", rec1.Code)
	}
	// Multi-node path does not set an ETag header.
	if rec1.Header().Get("ETag") != "" {
		t.Fatalf("multi-node poll must not advertise an ETag")
	}

	// Even echoing a plausible validator must not produce a 304.
	rec2 := doList(h, `"v0-h0-n0"`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("multi-node poll with validator status = %d, want 200 (never 304)", rec2.Code)
	}
}

func TestParseETagVersion(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want uint64
	}{
		{"empty", "", 0},
		{"quoted full validator", `"v42-h1700000000000000000-n3"`, 42},
		{"zero version", `"v0-h0-n0"`, 0},
		{"weak validator prefix", `W/"v7-h0-n0"`, 7},
		{"missing v prefix", `"42-h0-n0"`, 0},
		{"garbage", `"deadbeef"`, 0},
		{"no suffix", `"v15"`, 15},
		{"unquoted", `v9-h0-n0`, 9},
		{"non-numeric version", `"vNaN-h0-n0"`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseETagVersion(tc.in); got != tc.want {
				t.Errorf("parseETagVersion(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
