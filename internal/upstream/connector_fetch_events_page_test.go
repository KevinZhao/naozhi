package upstream

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

const pageKey = "feishu:direct:alice:general"

// pagedConnector serves one session whose log holds a visible entry at each
// of times.
func pagedConnector(t *testing.T, times ...int64) *Connector {
	t.Helper()
	router := makeRouter()
	proc := session.NewTestProcess()
	for _, ts := range times {
		proc.EventLog.Append(clievent.EventEntry{Time: ts, UUID: strconv.FormatInt(ts, 10), Type: "text", Summary: "x"})
	}
	router.InjectSession(pageKey, proc)
	return New(&Config{URL: "wss://x", NodeID: "n", Token: "t"}, testRouter(router), nil, nil, Discovery{})
}

func fetchEventsRaw(t *testing.T, c *Connector, params map[string]any) json.RawMessage {
	t.Helper()
	params["key"] = pageKey
	raw, _ := json.Marshal(params)
	result, err := c.handleRequest(context.Background(), context.Background(), node.ReverseMsg{Method: "fetch_events", Params: raw}, &sync.WaitGroup{})
	if err != nil {
		t.Fatalf("fetch_events %v: %v", params, err)
	}
	return result
}

func decodePage(t *testing.T, raw json.RawMessage) eventsPageResult {
	t.Helper()
	var page struct {
		Events  []clievent.EventEntry `json:"events"`
		HasMore *bool                 `json:"has_more"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("want a {events, has_more} page, got %s: %v", raw, err)
	}
	if page.Events == nil || page.HasMore == nil {
		t.Fatalf("page %s must carry both events and has_more", raw)
	}
	return eventsPageResult{Events: page.Events, HasMore: *page.HasMore}
}

func pageTimes(es []clievent.EventEntry) []int64 {
	out := make([]int64, 0, len(es))
	for _, e := range es {
		out = append(out, e.Time)
	}
	return out
}

func sameTimes(a, b []int64) bool {
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

// A `before` page is the "load earlier" page the dashboard's own events
// endpoint serves: the newest `limit` entries strictly older than before,
// with an authoritative has_more, and [] (not null) once history runs out.
func TestHandleRequest_FetchEvents_BeforePage(t *testing.T) {
	c := pagedConnector(t, 1000, 2000, 3000, 4000, 5000)
	for _, tc := range []struct {
		before  int64
		limit   int
		want    []int64
		hasMore bool
	}{
		{4000, 2, []int64{2000, 3000}, true},
		{3000, 5, []int64{1000, 2000}, false},
		{1000, 5, []int64{}, false},
	} {
		page := decodePage(t, fetchEventsRaw(t, c, map[string]any{"before": tc.before, "limit": tc.limit}))
		if got := pageTimes(page.Events); !sameTimes(got, tc.want) || page.HasMore != tc.hasMore {
			t.Errorf("before=%d limit=%d: got %v has_more=%v, want %v has_more=%v", tc.before, tc.limit, got, page.HasMore, tc.want, tc.hasMore)
		}
	}
}

// limit alone asks for the opening page, the same one a want_history
// subscribe gets.
func TestHandleRequest_FetchEvents_InitialPage(t *testing.T) {
	c := pagedConnector(t, 1000, 2000, 3000, 4000, 5000)
	page := decodePage(t, fetchEventsRaw(t, c, map[string]any{"limit": 2}))
	if got := pageTimes(page.Events); !sameTimes(got, []int64{4000, 5000}) || !page.HasMore {
		t.Fatalf("limit=2: got %v has_more=%v, want [4000 5000] has_more=true", got, page.HasMore)
	}
}

// Without before/limit, and for any catch-up (after > 0), the answer stays
// the bare array every primary decodes today.
func TestHandleRequest_FetchEvents_CatchUpStaysArray(t *testing.T) {
	c := pagedConnector(t, 1000, 2000, 3000)
	for _, params := range []map[string]any{
		{"after": 0},
		{"after": 2000, "limit": 1},
		{"after": 2000, "before": 3000},
	} {
		raw := fetchEventsRaw(t, c, params)
		var got []clievent.EventEntry
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("%v: want a bare array, got %s", params, raw)
		}
	}
}

// pageLimitSession records the limit the page readers are asked for.
type pageLimitSession struct {
	Session
	limit int
}

func (s *pageLimitSession) EventPageBeforeCtx(_ context.Context, _ int64, limit int) ([]clievent.EventEntry, bool) {
	s.limit = limit
	return nil, false
}

func (s *pageLimitSession) InitialHistoryPage(_ context.Context, limit int) ([]clievent.EventEntry, bool) {
	s.limit = limit
	return nil, false
}

// The page size is the primary's to ask but the node's to bound: a missing or
// oversized limit reads at most maxEventsPageLimit entries.
func TestEventsPage_ClampsLimit(t *testing.T) {
	for _, tc := range []struct {
		before int64
		limit  int
		want   int
	}{
		{5000, 0, maxEventsPageLimit},
		{5000, 1 << 30, maxEventsPageLimit},
		{5000, 7, 7},
		{0, 1 << 30, maxEventsPageLimit},
		{0, 7, 7},
	} {
		s := &pageLimitSession{}
		entries, _ := eventsPage(context.Background(), s, tc.before, tc.limit)
		if s.limit != tc.want {
			t.Errorf("before=%d limit=%d: reader asked for %d, want %d", tc.before, tc.limit, s.limit, tc.want)
		}
		if entries == nil {
			t.Errorf("before=%d limit=%d: an empty page must encode as [], not null", tc.before, tc.limit)
		}
	}
}
