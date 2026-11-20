package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
)

// stubPutAway is a record source with one put-away session.
type stubPutAway struct {
	key  string
	all  []clievent.EventEntry
	asks [][2]int64 // before, limit
}

func (s *stubPutAway) Events(_ context.Context, key string, before int64, limit int) ([]clievent.EventEntry, bool, bool) {
	if key != s.key {
		return nil, false, false
	}
	s.asks = append(s.asks, [2]int64{before, int64(limit)})
	var out []clievent.EventEntry
	for _, e := range s.all {
		if before == 0 || e.Time < before {
			out = append(out, e)
		}
	}
	more := len(out) > limit
	if more {
		out = out[len(out)-limit:]
	}
	return out, more, true
}

// A dispatched session put away is out of the router, yet its record is
// what a done task's card opens on: the events route serves it from the
// kept log, a later poll finds nothing new, and a key that is no session
// put away is still a 404 (planner RFC D136).
func TestHandleEvents_PutAwayRecord(t *testing.T) {
	const key = "task:aaaaaaaaaaaa"
	r := sessionpkg.NewRouter(sessionpkg.RouterConfig{MaxProcs: 4, StorePath: filepath.Join(t.TempDir(), "sessions.json")})
	t.Cleanup(r.Shutdown)
	src := &stubPutAway{key: key, all: []clievent.EventEntry{
		{Time: 1000, UUID: "u1", Type: "user", Summary: "查容量"},
		{Time: 2000, UUID: "t1", Type: "text", Summary: "查到了"},
		{Time: 3000, UUID: "r1", Type: "result"},
	}}
	h := New(Deps{Router: realRouter{r}, PutAway: src})
	get := func(q string) (*httptest.ResponseRecorder, []map[string]any) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.HandleEvents(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/events?key="+q, nil))
		var body []map[string]any
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("%s: body %s: %v", q, rec.Body, err)
			}
		}
		return rec, body
	}

	rec, body := get(key + "&limit=2")
	if rec.Code != http.StatusOK || len(body) != 2 || body[0]["uuid"] != "t1" || rec.Header().Get("X-Events-Has-More") != "1" {
		t.Fatalf("latest page: %d %s has-more %q", rec.Code, rec.Body, rec.Header().Get("X-Events-Has-More"))
	}
	rec, body = get(key + "&before=2000&limit=2")
	if rec.Code != http.StatusOK || len(body) != 1 || body[0]["uuid"] != "u1" || rec.Header().Get("X-Events-Has-More") != "0" {
		t.Fatalf("older page: %d %s has-more %q", rec.Code, rec.Body, rec.Header().Get("X-Events-Has-More"))
	}
	// The pane's poll: nothing is added to a record put away.
	asked := len(src.asks)
	rec, body = get(key + "&after=3000")
	if rec.Code != http.StatusOK || len(body) != 0 {
		t.Fatalf("catch-up: %d %s", rec.Code, rec.Body)
	}
	if len(src.asks) != asked+1 {
		t.Errorf("the catch-up did not check the key is put away")
	}

	if rec, _ := get("task:bbbbbbbbbbbb&limit=2"); rec.Code != http.StatusNotFound {
		t.Errorf("a key not put away: %d, want 404", rec.Code)
	}
	if rec := httptest.NewRecorder(); true {
		New(Deps{Router: realRouter{r}}).HandleEvents(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/events?key="+key+"&limit=2", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("no record source: %d, want 404", rec.Code)
		}
	}
}
