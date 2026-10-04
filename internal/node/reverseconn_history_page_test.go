package node

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/testhelper"
)

type fetchEventsParams struct {
	Key    string `json:"key"`
	After  int64  `json:"after"`
	Before int64  `json:"before"`
	Limit  int    `json:"limit"`
}

// readFetchEvents reads the next fetch_events request the primary sent.
func readFetchEvents(t *testing.T, ws *websocket.Conn) (string, fetchEventsParams) {
	t.Helper()
	req := readNodeFrame(t, ws, "request")
	if req.Method != "fetch_events" {
		t.Fatalf("request method = %q, want fetch_events", req.Method)
	}
	var p fetchEventsParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		t.Fatal(err)
	}
	return req.ReqID, p
}

func respond(t *testing.T, ws *websocket.Conn, reqID string, result any, errText string) {
	t.Helper()
	resp := ReverseMsg{Type: "response", ReqID: reqID, Error: errText}
	if result != nil {
		raw, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		resp.Result = raw
	}
	if err := ws.WriteJSON(resp); err != nil {
		t.Fatal(err)
	}
}

func jsonFrames(s *mockSink) []ServerMsg {
	var out []ServerMsg
	for _, m := range s.JSONMsgs() {
		if f, ok := m.(ServerMsg); ok {
			out = append(out, f)
		}
	}
	return out
}

// FetchEventsPage asks for before/limit; a node that pages answers with an
// object carrying has_more, an older node with its bare log.
func TestReverseConn_FetchEventsPage_DecodesBothAnswers(t *testing.T) {
	rc, ws, cleanup := setupReverseConnPair(t)
	defer cleanup()
	more := true
	for _, tc := range []struct {
		name    string
		answer  any
		hasMore *bool
	}{
		{"paging node", map[string]any{"events": []clievent.EventEntry{{Time: 1}, {Time: 2}}, "has_more": true}, &more},
		{"older node", []clievent.EventEntry{{Time: 1}, {Time: 2}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type result struct {
				page EventsPage
				err  error
			}
			got := make(chan result, 1)
			go func() {
				page, err := rc.FetchEventsPage(context.Background(), graceKey, EventsQuery{Before: 9, Limit: 4})
				got <- result{page, err}
			}()
			reqID, p := readFetchEvents(t, ws)
			if p.Key != graceKey || p.Before != 9 || p.Limit != 4 || p.After != 0 {
				t.Errorf("params = %+v, want {key %q before 9 limit 4}", p, graceKey)
			}
			respond(t, ws, reqID, tc.answer, "")
			res := <-got
			if res.err != nil {
				t.Fatal(res.err)
			}
			page := res.page
			if len(page.Events) != 2 || page.Events[1].Time != 2 {
				t.Fatalf("events = %+v, want times [1 2]", page.Events)
			}
			if (page.HasMore == nil) != (tc.hasMore == nil) || (page.HasMore != nil && *page.HasMore != *tc.hasMore) {
				t.Fatalf("HasMore = %v, want %v", page.HasMore, tc.hasMore)
			}
		})
	}
}

// A later sink on a key fetches its own opening page as a bounded page, so a
// long remote log no longer trips the RPC size cap; a page that knows
// has_more goes out even when empty, so the dashboard leaves its blank state.
func TestReverseConn_LaterSink_FetchesBoundedPage(t *testing.T) {
	f := newGraceFixture(t, 5*time.Second)
	ws, rc := f.dialCaps(t, historyCaps)
	rc.Subscribe(&mockSink{id: 1}, graceKey, 0, 50)
	readNodeFrame(t, ws, "subscribe")

	later := &mockSink{id: 2}
	rc.Subscribe(later, graceKey, 0, 30)
	reqID, p := readFetchEvents(t, ws)
	if p.After != 0 || p.Before != 0 || p.Limit != 30 {
		t.Fatalf("later sink's fetch = %+v, want an opening page of limit 30", p)
	}
	respond(t, ws, reqID, map[string]any{"events": []clievent.EventEntry{}, "has_more": false}, "")
	testhelper.Eventually(t, func() bool { return later.JSONMsgCount() == 2 }, 3*time.Second, "ack and page")
	frames := jsonFrames(later)
	if frames[0].Type != "subscribed" {
		t.Fatalf("first frame = %q, want subscribed", frames[0].Type)
	}
	if page := frames[1]; page.Type != "history" || !page.Initial || page.HasMore == nil || *page.HasMore {
		t.Fatalf("page = %+v, want an empty initial history with has_more=false", page)
	}
}

// A history fetch that fails tells the sink "history unavailable", which the
// dashboard renders as a retry, instead of leaving the pane blank.
func TestReverseConn_HistoryFetchFails_KeyedError(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps []string
	}{
		{"later sink", historyCaps},
		{"first sink on an older node", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGraceFixture(t, 5*time.Second)
			ws, rc := f.dialCaps(t, tc.caps)
			sink := &mockSink{id: 2}
			// With the cap the first sink needs no fetch: the later sink's
			// request is the only one.
			if tc.caps != nil {
				rc.Subscribe(&mockSink{id: 1}, graceKey, 0, 50)
			}
			rc.Subscribe(sink, graceKey, 0, 50)
			readNodeFrame(t, ws, "subscribe")
			reqID, _ := readFetchEvents(t, ws)
			respond(t, ws, reqID, nil, "reverse rpc response too large")
			testhelper.Eventually(t, func() bool { return sink.JSONMsgCount() == 1 }, 3*time.Second, "keyed error")
			got := jsonFrames(sink)[0]
			if got.Type != "error" || got.Key != graceKey || got.Node != "node-1" || got.Error != errHistoryUnavailable {
				t.Fatalf("frame = %+v, want error{key %q node node-1 %q}", got, graceKey, errHistoryUnavailable)
			}
		})
	}
}

// A fetch the link's drop cuts short reads as a reconnect, not as missing
// history: the adoption after the redial serves the sink again.
func TestReverseConn_HistoryFetchCutByDrop_Reconnecting(t *testing.T) {
	f := newGraceFixture(t, 5*time.Second)
	ws, rc := f.dial(t)
	sink := &mockSink{id: 1}
	rc.Subscribe(sink, graceKey, 0, 50)
	readNodeFrame(t, ws, "subscribe")
	readFetchEvents(t, ws)
	ws.Close()
	testhelper.Eventually(t, func() bool { return sink.JSONMsgCount() == 1 }, 3*time.Second, "keyed error")
	if got := jsonFrames(sink)[0]; got.Type != "error" || got.Error != errNodeReconnecting {
		t.Fatalf("frame = %+v, want error %q", got, errNodeReconnecting)
	}
}

// closeDoneOnly is the first half of markDisconnected: done closes while
// baseCtx is still live.
func closeDoneOnly(rc *ReverseConn) {
	rc.closeMu.Lock()
	rc.closed = true
	close(rc.done)
	rc.closeMu.Unlock()
}

// A fetch the drop cuts between done closing and baseCtx's cancel still reads
// as a reconnect.
func TestReverseConn_HistoryFetchCutBeforeCancel_Reconnecting(t *testing.T) {
	f := newGraceFixture(t, 5*time.Second)
	ws, rc := f.dial(t)
	sink := &mockSink{id: 1}
	rc.Subscribe(sink, graceKey, 0, 50)
	readNodeFrame(t, ws, "subscribe")
	readFetchEvents(t, ws)
	closeDoneOnly(rc)
	testhelper.Eventually(t, func() bool { return sink.JSONMsgCount() == 1 }, 3*time.Second, "keyed error")
	if got := jsonFrames(sink)[0]; got.Type != "error" || got.Error != errNodeReconnecting {
		t.Fatalf("frame = %+v, want error %q", got, errNodeReconnecting)
	}
}

// A catch-up cut the same way stays quiet: the next adoption catches up.
func TestReverseConn_CatchUpCutBeforeCancel_NoError(t *testing.T) {
	f := newGraceFixture(t, 5*time.Second)
	ws, rc := f.dial(t)
	sink := &mockSink{id: 1}
	subscribeOnLink(t, rc, ws, sink, 300)
	closeDoneOnly(rc)
	rc.subWG.Add(1)
	rc.catchUp(graceKey, 300)
	if n := sink.RawMsgCount(); n != 0 {
		t.Fatalf("sink got %d raw frames, want none: %+v", n, rawFrames(t, sink))
	}
}

// A failed catch-up after a redial leaves a gap in every sink's pane, so each
// is told the history is unavailable.
func TestReverseServer_CatchUpFails_KeyedError(t *testing.T) {
	f := newGraceFixture(t, 5*time.Second)
	ws1, rc1 := f.dial(t)
	sink := &mockSink{id: 1}
	subscribeOnLink(t, rc1, ws1, sink, 300)
	ws1.Close()
	f.waitLingering(t)

	ws2, _ := f.dial(t)
	readNodeFrame(t, ws2, "subscribe")
	reqID, p := readFetchEvents(t, ws2)
	if p.After != 300 {
		t.Fatalf("catch-up after = %d, want 300", p.After)
	}
	respond(t, ws2, reqID, nil, "session not found")
	testhelper.Eventually(t, func() bool { return sink.RawMsgCount() == 1 }, 3*time.Second, "keyed error")
	if got := rawFrames(t, sink)[0]; got.Type != "error" || got.Key != graceKey || got.Error != errHistoryUnavailable {
		t.Fatalf("frame = %+v, want error{key %q %q}", got, graceKey, errHistoryUnavailable)
	}
}

// The dashboard matches the error text literally.
func TestHistoryUnavailable_MatchesDashboard(t *testing.T) {
	src, err := os.ReadFile("../server/static/session_list.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "msg.error === '"+errHistoryUnavailable+"'") {
		t.Fatalf("session_list.js does not match the %q error", errHistoryUnavailable)
	}
}
