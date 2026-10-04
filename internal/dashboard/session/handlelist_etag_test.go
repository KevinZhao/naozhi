package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/node"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
)

// noNodeAccessor is a single-node NodeAccessor stub: zero known nodes, so
// HandleList takes the buildLocalResp path.
type noNodeAccessor struct{}

func (noNodeAccessor) HasNodes() bool                                           { return false }
func (noNodeAccessor) NodesSnapshot() map[string]node.Conn                      { return nil }
func (noNodeAccessor) NodeByID(string) (node.Conn, bool)                        { return nil, false }
func (noNodeAccessor) LookupNode(http.ResponseWriter, string) (node.Conn, bool) { return nil, false }
func (noNodeAccessor) KnownNodes() map[string]string                            { return nil }

// multiNodeAccessor reports a known but disconnected node, so HandleList
// takes the buildMultiNodeResp path.
type multiNodeAccessor struct{ noNodeAccessor }

func (multiNodeAccessor) KnownNodes() map[string]string { return map[string]string{"peer": "Peer"} }

// listRouter serves a fixed snapshot list at a fixed version: the tests change
// the sessions without moving the version, as a turn end does.
type listRouter struct {
	*fakeRouter
	snaps   []sessionpkg.SessionSnapshot
	version uint64
}

// ListSessionsWithVersion returns a copy: HandleList compacts and sorts it.
func (l *listRouter) ListSessionsWithVersion() ([]sessionpkg.SessionSnapshot, uint64) {
	return slices.Clone(l.snaps), l.version
}

func newListRouter(keys ...string) *listRouter {
	l := &listRouter{fakeRouter: newFakeRouter(), version: 7}
	for _, k := range keys {
		l.snaps = append(l.snaps, sessionpkg.SessionSnapshot{Key: k, State: "ready"})
	}
	return l
}

func newETagTestHandlers(t *testing.T, r RouterView, na NodeAccessor) *Handlers {
	t.Helper()
	return New(Deps{
		Router:        r,
		NodeAccess:    na,
		NodeCache:     node.NewCacheManager(func() map[string]node.Conn { return nil }, func() {}),
		StartedAt:     time.Now(),
		WatchdogNoOut: &atomic.Int64{},
		WatchdogTotal: &atomic.Int64{},
		// ClaudeDir empty: historySessions() returns nil without a scan.
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

// firstETag polls without a validator and returns the advertised ETag.
func firstETag(t *testing.T, h *Handlers) string {
	t.Helper()
	rec := doList(h, "")
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("first poll: status %d, body %d bytes; want a 200 body", rec.Code, rec.Body.Len())
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("first poll did not set an ETag")
	}
	return etag
}

func wantNotModified(t *testing.T, rec *httptest.ResponseRecorder, etag string) {
	t.Helper()
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("304 body = %d bytes, want none", rec.Body.Len())
	}
	if got := rec.Header().Get("ETag"); got != etag {
		t.Fatalf("304 ETag = %q, want %q", got, etag)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("304 Cache-Control = %q, want no-store", got)
	}
}

func wantFreshBody(t *testing.T, rec *httptest.ResponseRecorder, stale string) {
	t.Helper()
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("status %d, body %d bytes; want a 200 body", rec.Code, rec.Body.Len())
	}
	if got := rec.Header().Get("ETag"); got == "" || got == stale {
		t.Fatalf("ETag = %q, want a new validator (stale %q)", got, stale)
	}
}

func TestHandleList_IdenticalBodyReturns304(t *testing.T) {
	h := newETagTestHandlers(t, newListRouter("feishu:direct:a:general"), noNodeAccessor{})
	etag := firstETag(t, h)
	wantNotModified(t, doList(h, etag), etag)
}

// TestHandleList_StateChangeAtSameVersionRebuilds is the regression the old
// storeGen validator failed: running/ready, effort and spawn diagnostics move
// without a version bump, and each must change the ETag.
func TestHandleList_StateChangeAtSameVersionRebuilds(t *testing.T) {
	mutations := map[string]func(*sessionpkg.SessionSnapshot){
		"state":       func(s *sessionpkg.SessionSnapshot) { s.State = "running" },
		"effort":      func(s *sessionpkg.SessionSnapshot) { s.Effort = "high" },
		"last reply":  func(s *sessionpkg.SessionSnapshot) { s.LastResponse = "done" },
		"total cost":  func(s *sessionpkg.SessionSnapshot) { s.TotalCost = 0.5 },
		"last active": func(s *sessionpkg.SessionSnapshot) { s.LastActive = 42 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			r := newListRouter("feishu:direct:a:general")
			h := newETagTestHandlers(t, r, noNodeAccessor{})
			etag := firstETag(t, h)
			mutate(&r.snaps[0])
			rec := doList(h, etag)
			wantFreshBody(t, rec, etag)
			wantNotModified(t, doList(h, rec.Header().Get("ETag")), rec.Header().Get("ETag"))
		})
	}
}

func TestHandleList_VersionBumpRebuilds(t *testing.T) {
	r := newListRouter("feishu:direct:a:general")
	h := newETagTestHandlers(t, r, noNodeAccessor{})
	etag := firstETag(t, h)
	r.version++
	wantFreshBody(t, doList(h, etag), etag)
}

// TestHandleList_UptimeAloneKeeps304: the one per-second field is left out of
// the validator, single-node and multi-node, and the 200 body still carries it.
func TestHandleList_UptimeAloneKeeps304(t *testing.T) {
	for name, na := range map[string]NodeAccessor{"single-node": noNodeAccessor{}, "multi-node": multiNodeAccessor{}} {
		t.Run(name, func(t *testing.T) {
			h := newETagTestHandlers(t, newListRouter("feishu:direct:a:general"), na)
			first := doList(h, "")
			etag := first.Header().Get("ETag")
			h.deps.StartedAt = h.deps.StartedAt.Add(-2 * time.Hour)
			wantNotModified(t, doList(h, etag), etag)

			second := doList(h, "")
			if got := second.Header().Get("ETag"); got != etag {
				t.Fatalf("ETag moved with uptime alone: %q -> %q", etag, got)
			}
			if uptimeOf(t, first) == uptimeOf(t, second) {
				t.Fatalf("uptime did not move (%q); the test proves nothing", uptimeOf(t, first))
			}
		})
	}
}

func uptimeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Stats struct {
			Uptime string `json:"uptime"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body.Stats.Uptime
}

// TestHandleList_TableOrderDoesNotMoveETag: the session table yields map
// order, so the same sessions in another order must still 304.
func TestHandleList_TableOrderDoesNotMoveETag(t *testing.T) {
	r := newListRouter("feishu:direct:a:general", "feishu:direct:b:general", "feishu:direct:c:general")
	h := newETagTestHandlers(t, r, noNodeAccessor{})
	etag := firstETag(t, h)
	slices.Reverse(r.snaps)
	wantNotModified(t, doList(h, etag), etag)
}

func TestHandleList_HistoryChangeRebuilds(t *testing.T) {
	h := newETagTestHandlers(t, newListRouter("feishu:direct:a:general"), noNodeAccessor{})
	h.deps.ClaudeDir = t.TempDir()
	setHistory := func(ids ...string) {
		var hist []discovery.RecentSession
		for _, id := range ids {
			hist = append(hist, discovery.RecentSession{SessionID: id, LastActive: 1})
		}
		h.historyCacheMu.Lock()
		h.SetCachedHistoryForTest(hist, time.Now())
		h.historyCacheMu.Unlock()
	}
	setHistory("s1")
	etag := firstETag(t, h)
	// A rescan with identical entries keeps the validator.
	setHistory("s1")
	wantNotModified(t, doList(h, etag), etag)
	setHistory("s1", "s2")
	wantFreshBody(t, doList(h, etag), etag)
}

func TestHandleList_IfNoneMatchForms(t *testing.T) {
	h := newETagTestHandlers(t, newListRouter("feishu:direct:a:general"), noNodeAccessor{})
	etag := firstETag(t, h)
	strong, ok := strings.CutPrefix(etag, "W/")
	if !ok {
		t.Fatalf("ETag %q is not weak; bodies differing in uptime share it", etag)
	}
	for name, tc := range map[string]struct {
		header string
		want   int
	}{
		"as served":       {etag, http.StatusNotModified},
		"strong form":     {strong, http.StatusNotModified},
		"in a list":       {`"other", ` + etag, http.StatusNotModified},
		"wildcard":        {"*", http.StatusNotModified},
		"other tag":       {`W/"b00000000000000000000000000000000"`, http.StatusOK},
		"old gen format":  {`"v7-h0-n0"`, http.StatusOK},
		"unquoted":        {strings.Trim(strong, `"`), http.StatusOK},
		"garbled list":    {`,,"x" ,W/`, http.StatusOK},
		"weak other":      {`W/"other"`, http.StatusOK},
		"etag as a token": {"x" + strong, http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			if got := doList(h, tc.header).Code; got != tc.want {
				t.Fatalf("If-None-Match %q: status %d, want %d", tc.header, got, tc.want)
			}
		})
	}
}

// TestHandleList_MultiNodeIdenticalBodyReturns304: the multi-node body is
// covered by the same validator.
func TestHandleList_MultiNodeIdenticalBodyReturns304(t *testing.T) {
	h := newETagTestHandlers(t, newListRouter("feishu:direct:a:general"), multiNodeAccessor{})
	etag := firstETag(t, h)
	wantNotModified(t, doList(h, etag), etag)
}

// fakePeer is a connected remote node serving one session and one project.
type fakePeer struct {
	node.Conn
	id string
}

func (p fakePeer) NodeID() string      { return p.id }
func (p fakePeer) DisplayName() string { return "Peer " + p.id }
func (p fakePeer) RemoteAddr() string  { return "" }
func (p fakePeer) FetchSessions(context.Context) ([]map[string]any, error) {
	return []map[string]any{{"key": "feishu:direct:" + p.id + ":general"}}, nil
}
func (p fakePeer) FetchProjects(context.Context) ([]map[string]any, error) {
	return []map[string]any{{"name": "proj-" + p.id}}, nil
}
func (p fakePeer) FetchDiscovered(context.Context) ([]map[string]any, error) { return nil, nil }

type peersAccessor struct {
	noNodeAccessor
	peers map[string]node.Conn
}

func (a peersAccessor) HasNodes() bool                      { return true }
func (a peersAccessor) NodesSnapshot() map[string]node.Conn { return a.peers }
func (a peersAccessor) KnownNodes() map[string]string {
	known := make(map[string]string, len(a.peers))
	for id := range a.peers {
		known[id] = "Peer " + id
	}
	return known
}

// TestHandleList_PeerOrderDoesNotMoveETag: remote sessions and projects are
// merged per node, and node maps iterate in random order; with eight peers an
// unsorted merge would almost surely reorder within twenty polls.
func TestHandleList_PeerOrderDoesNotMoveETag(t *testing.T) {
	peers := make(map[string]node.Conn)
	for i := range 8 {
		id := fmt.Sprintf("n%d", i)
		peers[id] = fakePeer{id: id}
	}
	h := newETagTestHandlers(t, newListRouter("feishu:direct:a:general"), peersAccessor{peers: peers})
	cache := node.NewCacheManager(func() map[string]node.Conn { return peers }, func() {})
	cache.RefreshAll()
	h.deps.NodeCache = cache

	first := doList(h, "")
	for _, want := range []string{`"feishu:direct:n7:general"`, `"proj-n7"`} {
		if !strings.Contains(first.Body.String(), want) {
			t.Fatalf("body lacks %s; the peers were not merged", want)
		}
	}
	etag := first.Header().Get("ETag")
	for range 20 {
		wantNotModified(t, doList(h, etag), etag)
	}
}
