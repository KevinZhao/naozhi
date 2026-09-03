package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/node"
)

// fakeEventsConn is a node.Conn whose only live method is FetchEvents; the
// embedded nil interface covers the rest (never called by HandleEvents).
type fakeEventsConn struct {
	node.Conn
	entries  []cli.EventEntry
	gotAfter int64
}

func (c *fakeEventsConn) FetchEvents(_ context.Context, _ string, after int64) ([]cli.EventEntry, error) {
	c.gotAfter = after
	return c.entries, nil
}

type fakeEventsNodeAccessor struct {
	noNodeAccessor
	conn node.Conn
}

func (a fakeEventsNodeAccessor) LookupNode(http.ResponseWriter, string) (node.Conn, bool) {
	return a.conn, true
}

func remoteEventsFixture(n int) []cli.EventEntry {
	out := make([]cli.EventEntry, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, cli.EventEntry{Time: int64(i), Type: "text", Summary: "e"})
	}
	return out
}

func doRemoteEvents(t *testing.T, h *Handlers, query string) (*httptest.ResponseRecorder, []cli.EventEntry) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/events?key=feishu:p2p:u1:general&node=peer"+query, nil)
	rec := httptest.NewRecorder()
	h.HandleEvents(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got []cli.EventEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return rec, got
}

func times(es []cli.EventEntry) []int64 {
	out := make([]int64, 0, len(es))
	for _, e := range es {
		out = append(out, e.Time)
	}
	return out
}

func equalTimes(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// #2433 P2: the node RPC only carries `after`, so the remote proxy branch used
// to ignore `before` and always return the NEWEST `limit` entries. The
// dashboard's "load earlier" then prepended the same page forever. The proxy
// must filter Time < before locally, take the tail `limit`, and report
// X-Events-Has-More so the client knows when to stop.
func TestHandleEvents_RemoteBefore_FiltersAndPaginates(t *testing.T) {
	conn := &fakeEventsConn{entries: remoteEventsFixture(10)}
	h := newIfChangedTestHandlers(t, fakeEventsNodeAccessor{conn: conn})

	rec, got := doRemoteEvents(t, h, "&before=6&limit=2")
	if want := []int64{4, 5}; !equalTimes(times(got), want) {
		t.Fatalf("before=6 limit=2: got times %v want %v", times(got), want)
	}
	if hm := rec.Header().Get("X-Events-Has-More"); hm != "1" {
		t.Errorf("X-Events-Has-More=%q want 1 (entries 1..3 remain older than the page)", hm)
	}
	if conn.gotAfter != 0 {
		t.Errorf("remote after=%d want 0 (before must not be forwarded as after)", conn.gotAfter)
	}

	rec, got = doRemoteEvents(t, h, "&before=6&limit=5")
	if want := []int64{1, 2, 3, 4, 5}; !equalTimes(times(got), want) {
		t.Fatalf("before=6 limit=5: got times %v want %v", times(got), want)
	}
	if hm := rec.Header().Get("X-Events-Has-More"); hm != "0" {
		t.Errorf("X-Events-Has-More=%q want 0 (page reached the oldest entry)", hm)
	}
}

// Paging past the oldest remote entry must yield an empty array (the client
// maps that to "done") and has-more=0 — never the newest page again.
func TestHandleEvents_RemoteBefore_ExhaustedReturnsEmpty(t *testing.T) {
	conn := &fakeEventsConn{entries: remoteEventsFixture(10)}
	h := newIfChangedTestHandlers(t, fakeEventsNodeAccessor{conn: conn})

	rec, got := doRemoteEvents(t, h, "&before=1&limit=100")
	if len(got) != 0 {
		t.Fatalf("before=1: got %d entries %v want 0", len(got), times(got))
	}
	if body := rec.Body.String(); body != "[]\n" && body != "[]" {
		t.Errorf("body=%q want an empty JSON array (client treats null as done too, but [] is the contract)", body)
	}
	if hm := rec.Header().Get("X-Events-Has-More"); hm != "0" {
		t.Errorf("X-Events-Has-More=%q want 0", hm)
	}
}

// `before` with no limit uses the same page cap as the local branch.
func TestHandleEvents_RemoteBefore_NoLimitUsesPageCap(t *testing.T) {
	conn := &fakeEventsConn{entries: remoteEventsFixture(maxEventsPageLimit + 5)}
	h := newIfChangedTestHandlers(t, fakeEventsNodeAccessor{conn: conn})

	rec, got := doRemoteEvents(t, h, "&before=1000000")
	if len(got) != maxEventsPageLimit {
		t.Fatalf("before w/o limit: got %d entries want %d", len(got), maxEventsPageLimit)
	}
	if got[len(got)-1].Time != int64(maxEventsPageLimit+5) {
		t.Errorf("tail must keep the newest of the filtered set; last time=%d", got[len(got)-1].Time)
	}
	if hm := rec.Header().Get("X-Events-Has-More"); hm != "1" {
		t.Errorf("X-Events-Has-More=%q want 1", hm)
	}
}

// Regression guard for the untouched paths: initial fetch (limit only) still
// tails the newest entries without a has-more header (client falls back to
// its length heuristic for remote nodes), and `after` still wins over
// `before` per the documented precedence.
func TestHandleEvents_RemoteInitialAndAfter_Unchanged(t *testing.T) {
	conn := &fakeEventsConn{entries: remoteEventsFixture(10)}
	h := newIfChangedTestHandlers(t, fakeEventsNodeAccessor{conn: conn})

	rec, got := doRemoteEvents(t, h, "&limit=3")
	if want := []int64{8, 9, 10}; !equalTimes(times(got), want) {
		t.Fatalf("limit=3: got times %v want %v", times(got), want)
	}
	if _, ok := rec.Header()["X-Events-Has-More"]; ok {
		t.Errorf("initial remote page must not claim an authoritative has-more header")
	}

	_, got = doRemoteEvents(t, h, "&after=7&before=3&limit=100")
	if conn.gotAfter != 7 {
		t.Errorf("remote after=%d want 7", conn.gotAfter)
	}
	if len(got) != 10 {
		t.Errorf("after wins over before: got %d entries want all 10 unfiltered", len(got))
	}
}
