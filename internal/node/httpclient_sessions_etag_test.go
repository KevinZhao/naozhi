package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

// etagSessionsServer serves /api/sessions from bodies[i] and etags[i] on the
// i-th request (the last pair repeats), answers a matching If-None-Match with
// 304, and records each request's If-None-Match. Other paths are 404.
type etagSessionsServer struct {
	mu     sync.Mutex
	bodies [][]map[string]any
	etags  []string
	status []int // optional per-request status override; 0 means normal
	inm    []string
	calls  int
}

func (s *etagSessionsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/sessions" {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	i := s.calls
	s.calls++
	s.inm = append(s.inm, r.Header.Get("If-None-Match"))
	pick := min(i, len(s.bodies)-1)
	body, etag := s.bodies[pick], s.etags[pick]
	status := 0
	if i < len(s.status) {
		status = s.status[i]
	}
	s.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if etag != "" {
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"sessions": body})
}

func (s *etagSessionsServer) sentINM() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.inm...)
}

func fetchOK(t *testing.T, c *HTTPClient) []map[string]any {
	t.Helper()
	got, err := c.FetchSessions(context.Background())
	if err != nil {
		t.Fatalf("FetchSessions: %v", err)
	}
	return got
}

func TestHTTPClient_FetchSessions_conditional304(t *testing.T) {
	body := []map[string]any{{"key": "a", "state": "ready", "meta": map[string]any{"n": 1.0}}}
	es := &etagSessionsServer{bodies: [][]map[string]any{body}, etags: []string{`W/"bX"`}}
	srv := httptest.NewServer(es)
	defer srv.Close()
	c := newTestHTTPClient(t, srv, "tok")

	first := fetchOK(t, c)
	second := fetchOK(t, c)
	if got := es.sentINM(); !reflect.DeepEqual(got, []string{"", `W/"bX"`}) {
		t.Fatalf("If-None-Match sent = %q, want [\"\" W/\"bX\"]", got)
	}
	if !reflect.DeepEqual(first, body) || !reflect.DeepEqual(second, body) {
		t.Fatalf("results = %v / %v, want %v twice", first, second, body)
	}
}

func TestHTTPClient_FetchSessions_etagChanged(t *testing.T) {
	b1 := []map[string]any{{"key": "a"}}
	b2 := []map[string]any{{"key": "a"}, {"key": "b"}}
	es := &etagSessionsServer{
		bodies: [][]map[string]any{b1, b2},
		etags:  []string{`W/"b1"`, `W/"b2"`},
	}
	srv := httptest.NewServer(es)
	defer srv.Close()
	c := newTestHTTPClient(t, srv, "")

	fetchOK(t, c)
	if got := fetchOK(t, c); !reflect.DeepEqual(got, b2) {
		t.Fatalf("second fetch = %v, want the new body %v", got, b2)
	}
	if got := fetchOK(t, c); !reflect.DeepEqual(got, b2) {
		t.Fatalf("third fetch (304) = %v, want %v", got, b2)
	}
	if got := es.sentINM(); !reflect.DeepEqual(got, []string{"", `W/"b1"`, `W/"b2"`}) {
		t.Fatalf("If-None-Match sent = %q", got)
	}
}

func TestHTTPClient_FetchSessions_noETagNeverConditional(t *testing.T) {
	es := &etagSessionsServer{bodies: [][]map[string]any{{{"key": "a"}}}, etags: []string{""}}
	srv := httptest.NewServer(es)
	defer srv.Close()
	c := newTestHTTPClient(t, srv, "")

	for range 3 {
		fetchOK(t, c)
	}
	if got := es.sentINM(); !reflect.DeepEqual(got, []string{"", "", ""}) {
		t.Fatalf("If-None-Match sent = %q, want none", got)
	}
}

func TestHTTPClient_FetchSessions_errorResetsETag(t *testing.T) {
	body := []map[string]any{{"key": "a"}}
	es := &etagSessionsServer{
		bodies: [][]map[string]any{body},
		etags:  []string{`W/"bX"`},
		status: []int{0, http.StatusInternalServerError},
	}
	srv := httptest.NewServer(es)
	defer srv.Close()
	c := newTestHTTPClient(t, srv, "")

	fetchOK(t, c)
	if _, err := c.FetchSessions(context.Background()); err == nil {
		t.Fatal("expected an error on 500")
	}
	if got := fetchOK(t, c); !reflect.DeepEqual(got, body) {
		t.Fatalf("fetch after error = %v, want %v", got, body)
	}
	if got := es.sentINM(); !reflect.DeepEqual(got, []string{"", `W/"bX"`, ""}) {
		t.Fatalf("If-None-Match sent = %q; the request after a failure must be unconditional", got)
	}
}

func TestHTTPClient_FetchSessions_decodeErrorResetsETag(t *testing.T) {
	var calls atomic.Int32
	var lastINM atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastINM.Store(r.Header.Get("If-None-Match"))
		switch calls.Add(1) {
		case 1:
			w.Header().Set("ETag", `W/"b1"`)
			fmt.Fprint(w, `{"sessions":[{"key":"a"}]}`)
		case 2:
			w.Header().Set("ETag", `W/"b2"`)
			fmt.Fprint(w, `{"sessions":[`)
		default:
			w.Header().Set("ETag", `W/"b3"`)
			fmt.Fprint(w, `{"sessions":[]}`)
		}
	}))
	defer srv.Close()
	c := newTestHTTPClient(t, srv, "")

	fetchOK(t, c)
	if _, err := c.FetchSessions(context.Background()); err == nil {
		t.Fatal("expected a decode error on a truncated body")
	}
	fetchOK(t, c)
	if got := lastINM.Load(); got != "" {
		t.Fatalf("request after a decode error sent If-None-Match %q, want none", got)
	}
}

func TestHTTPClient_FetchSessions_unsolicited304IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	c := newTestHTTPClient(t, srv, "")

	got, err := c.FetchSessions(context.Background())
	if err == nil {
		t.Fatalf("304 to an unconditional GET returned %v, want an error", got)
	}
}

// CacheManager stamps rs["node"] on every returned map and publishes them to
// lock-free JSON encoders, so no two results may share a top-level map.
func TestHTTPClient_FetchSessions_resultIsolation(t *testing.T) {
	es := &etagSessionsServer{bodies: [][]map[string]any{{{"key": "a"}}}, etags: []string{`W/"bX"`}}
	srv := httptest.NewServer(es)
	defer srv.Close()
	c := newTestHTTPClient(t, srv, "")

	first := fetchOK(t, c)
	first[0]["node"] = "x"
	second := fetchOK(t, c)
	if _, ok := second[0]["node"]; ok {
		t.Fatalf("304 result carries a key the caller set on an earlier result: %v", second[0])
	}
	third := fetchOK(t, c)
	if reflect.ValueOf(second[0]).UnsafePointer() == reflect.ValueOf(third[0]).UnsafePointer() {
		t.Fatal("two 304 results share one map")
	}
}

// Run under -race: the CacheManager pattern of writing "node" on one result
// while another goroutine JSON-encodes an earlier one.
func TestHTTPClient_FetchSessions_concurrentRefresh(t *testing.T) {
	es := &etagSessionsServer{
		bodies: [][]map[string]any{{{"key": "a"}, {"key": "b"}}},
		etags:  []string{`W/"bX"`},
	}
	srv := httptest.NewServer(es)
	defer srv.Close()
	c := newTestHTTPClient(t, srv, "")

	published := make(chan []map[string]any, 64)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 8 {
				got, err := c.FetchSessions(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				for _, rs := range got {
					rs["node"] = "n1"
				}
				published <- got
			}
		})
	}
	var enc sync.WaitGroup
	enc.Go(func() {
		for got := range published {
			if _, err := json.Marshal(got); err != nil {
				t.Error(err)
			}
		}
	})
	wg.Wait()
	close(published)
	enc.Wait()
}

func TestCacheManager_HTTPNode304KeepsSessions(t *testing.T) {
	es := &etagSessionsServer{bodies: [][]map[string]any{{{"key": "a"}}}, etags: []string{`W/"bX"`}}
	srv := httptest.NewServer(es)
	defer srv.Close()
	c := newTestHTTPClient(t, srv, "")
	m := NewCacheManager(func() map[string]Conn { return map[string]Conn{"n1": c} }, nil)

	m.RefreshAll()
	m.RefreshAll()
	if got := es.sentINM(); len(got) != 2 || got[1] != `W/"bX"` {
		t.Fatalf("If-None-Match sent = %q, want the second poll conditional", got)
	}
	sessions, status := m.Sessions()
	if status["n1"] != "ok" {
		t.Fatalf("status after a 304 poll = %q, want ok", status["n1"])
	}
	if len(sessions["n1"]) != 1 || sessions["n1"][0]["key"] != "a" || sessions["n1"][0]["node"] != "n1" {
		t.Fatalf("sessions after a 304 poll = %v", sessions["n1"])
	}
}
