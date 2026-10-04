package node

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// FetchEventsPage asks a direct node's events endpoint for one page and reads
// its X-Events-Has-More; a node that sets no header leaves HasMore nil.
func TestHTTPClient_FetchEventsPage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		q       EventsQuery
		header  string
		query   url.Values
		hasMore *bool
	}{
		{"load earlier", EventsQuery{Before: 9, Limit: 4}, "1", url.Values{"key": {"k"}, "before": {"9"}, "limit": {"4"}}, new(true)},
		{"opening page", EventsQuery{Limit: 4}, "0", url.Values{"key": {"k"}, "limit": {"4"}}, new(false)},
		{"no header", EventsQuery{Limit: 4}, "", url.Values{"key": {"k"}, "limit": {"4"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got url.Values
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.Query()
				if tc.header != "" {
					w.Header().Set("X-Events-Has-More", tc.header)
				}
				_ = json.NewEncoder(w).Encode([]clievent.EventEntry{{Time: 3}})
			}))
			defer srv.Close()

			page, err := NewHTTPClient("n", srv.URL, "tok", "N").FetchEventsPage(context.Background(), "k", tc.q)
			if err != nil {
				t.Fatal(err)
			}
			if got.Encode() != tc.query.Encode() {
				t.Fatalf("query = %s, want %s", got.Encode(), tc.query.Encode())
			}
			if len(page.Events) != 1 || page.Events[0].Time != 3 {
				t.Fatalf("events = %+v", page.Events)
			}
			if (page.HasMore == nil) != (tc.hasMore == nil) || (page.HasMore != nil && *page.HasMore != *tc.hasMore) {
				t.Fatalf("HasMore = %v, want %v", page.HasMore, tc.hasMore)
			}
		})
	}
}

// relayWithEvents is a direct node whose WS side only authenticates and whose
// events endpoint is events.
func relayWithEvents(t *testing.T, events http.HandlerFunc) *wsRelay {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/sessions/events" {
			events(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		authHandshake(t, conn)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	relay := newWSRelay(newRelayNode(srv))
	t.Cleanup(relay.Close)
	return relay
}

// A later sink's own opening page is asked of the node with the browser's
// page size, and comes back with the node's has_more.
func TestWSRelay_LaterSink_FetchesBoundedPage(t *testing.T) {
	gotLimit := make(chan string, 1)
	relay := relayWithEvents(t, func(w http.ResponseWriter, r *http.Request) {
		gotLimit <- r.URL.Query().Get("limit")
		w.Header().Set("X-Events-Has-More", "1")
		_ = json.NewEncoder(w).Encode([]clievent.EventEntry{{Time: 5}})
	})
	relay.Subscribe(&mockSink{id: 1}, "key1", 0, 30)
	later := &mockSink{id: 2}
	relay.Subscribe(later, "key1", 0, 30)

	select {
	case l := <-gotLimit:
		if l != "30" {
			t.Fatalf("limit = %q, want 30", l)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no page request")
	}
	testhelper.Eventually(t, func() bool { return later.JSONMsgCount() == 2 }, 3*time.Second, "ack and page")
	page := jsonFrames(later)[1]
	if page.Type != "history" || !page.Initial || page.HasMore == nil || !*page.HasMore || len(page.Events) != 1 {
		t.Fatalf("page = %+v, want an initial history of 1 event with has_more=true", page)
	}
}

// A later sink whose page fetch fails is told the history is unavailable.
func TestWSRelay_LaterSink_FetchFails_KeyedError(t *testing.T) {
	relay := relayWithEvents(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream error", http.StatusBadGateway)
	})
	relay.Subscribe(&mockSink{id: 1}, "key1", 0, 30)
	later := &mockSink{id: 2}
	relay.Subscribe(later, "key1", 0, 30)

	testhelper.Eventually(t, func() bool { return later.JSONMsgCount() == 2 }, 3*time.Second, "ack and error")
	if got := jsonFrames(later)[1]; got.Type != "error" || got.Key != "key1" || got.Node != "test-node" || got.Error != errHistoryUnavailable {
		t.Fatalf("frame = %+v, want error{key key1 node test-node %q}", got, errHistoryUnavailable)
	}
}
