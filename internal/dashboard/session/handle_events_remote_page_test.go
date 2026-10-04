package session

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
)

func pagedConn(hasMore bool, times ...int64) *fakeEventsConn {
	events := make([]clievent.EventEntry, 0, len(times))
	for _, t := range times {
		events = append(events, clievent.EventEntry{Time: t, Type: "text", Summary: "e"})
	}
	return &fakeEventsConn{page: &node.EventsPage{Events: events, HasMore: &hasMore}}
}

// A node that pages answers the "load earlier" request itself: the proxy asks
// for one entry more than the page (for peers that cannot report has-more)
// and passes a has-more answer through untouched, as the local branch would
// serve it. Trimming the extra entry could split a millisecond the next
// strict `before` page then skips.
func TestHandleEvents_RemoteBefore_PagingNodeAnswersItself(t *testing.T) {
	for _, tc := range []struct {
		name    string
		hasMore bool
		header  string
	}{
		{"older history left", true, "1"},
		{"oldest page", false, "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := pagedConn(tc.hasMore, 3, 4, 5)
			h := newIfChangedTestHandlers(t, fakeEventsNodeAccessor{conn: conn})

			rec, got := doRemoteEvents(t, h, "&before=6&limit=2")
			if want := (node.EventsQuery{Before: 6, Limit: 3}); conn.gotQuery != want {
				t.Fatalf("page query = %+v, want %+v", conn.gotQuery, want)
			}
			if want := []int64{3, 4, 5}; !equalTimes(times(got), want) {
				t.Fatalf("got times %v, want the node's page %v", times(got), want)
			}
			if hm := rec.Header().Get("X-Events-Has-More"); hm != tc.header {
				t.Errorf("X-Events-Has-More=%q, want the node's %q", hm, tc.header)
			}
		})
	}
}

// A peer that honours before/limit but reports no has-more still yields one:
// it returned the extra entry asked for.
func TestHandleEvents_RemoteBefore_BoundedPeerWithoutHasMore(t *testing.T) {
	conn := &fakeEventsConn{entries: remoteEventsFixture(10), bounded: true}
	h := newIfChangedTestHandlers(t, fakeEventsNodeAccessor{conn: conn})

	rec, got := doRemoteEvents(t, h, "&before=6&limit=2")
	if want := []int64{4, 5}; !equalTimes(times(got), want) {
		t.Fatalf("got times %v want %v", times(got), want)
	}
	if hm := rec.Header().Get("X-Events-Has-More"); hm != "1" {
		t.Errorf("X-Events-Has-More=%q want 1 (the peer returned limit+1 entries)", hm)
	}

	rec, got = doRemoteEvents(t, h, "&before=3&limit=2")
	if want := []int64{1, 2}; !equalTimes(times(got), want) {
		t.Fatalf("got times %v want %v", times(got), want)
	}
	if hm := rec.Header().Get("X-Events-Has-More"); hm != "0" {
		t.Errorf("X-Events-Has-More=%q want 0", hm)
	}
}

// The initial page (limit only) is the node's own visible-aware page with
// its has-more, instead of a tail cut from the whole remote log.
func TestHandleEvents_RemoteInitial_PagingNodeAnswersItself(t *testing.T) {
	conn := pagedConn(true, 7, 8, 9, 10)
	h := newIfChangedTestHandlers(t, fakeEventsNodeAccessor{conn: conn})

	rec, got := doRemoteEvents(t, h, "&limit=3")
	if want := (node.EventsQuery{Limit: 3}); conn.gotQuery != want {
		t.Fatalf("page query = %+v, want %+v", conn.gotQuery, want)
	}
	if want := []int64{7, 8, 9, 10}; !equalTimes(times(got), want) {
		t.Fatalf("got times %v, want the node's page %v", times(got), want)
	}
	if hm := rec.Header().Get("X-Events-Has-More"); hm != "1" {
		t.Errorf("X-Events-Has-More=%q, want the node's 1", hm)
	}
}
