package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recv struct {
	mu     sync.Mutex
	bodies [][]byte
	heads  []http.Header
	status atomic.Int32 // response code; 0 = 200
	hits   atomic.Int32
}

func (r *recv) handler(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.bodies = append(r.bodies, b)
	r.heads = append(r.heads, req.Header.Clone())
	r.mu.Unlock()
	n := r.hits.Add(1)
	if st := int(r.status.Load()); st != 0 && (st != 503 || n == 1) {
		w.WriteHeader(st)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func noSleep(context.Context, time.Duration) {}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDeliver_SignedPayloadAndHeaders(t *testing.T) {
	r := &recv{}
	srv := httptest.NewServer(http.HandlerFunc(r.handler))
	defer srv.Close()
	s := New([]Endpoint{{URL: srv.URL, Secret: "s3cr3t"}}, WithSleep(noSleep))
	s.Deliver(Event{Type: EventRunEnded, Subsystem: "cron", OwnerID: "job1", RunID: "r1", State: "succeeded"})
	s.Close(context.Background())

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.bodies) != 1 {
		t.Fatalf("deliveries = %d", len(r.bodies))
	}
	var ev Event
	if err := json.Unmarshal(r.bodies[0], &ev); err != nil || ev.RunID != "r1" || ev.State != "succeeded" {
		t.Fatalf("body = %s err=%v", r.bodies[0], err)
	}
	h := r.heads[0]
	if h.Get("X-Naozhi-Event") != EventRunEnded || h.Get("X-Naozhi-Delivery") != "r1-run.ended" || h.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", h)
	}
	if !Verify("s3cr3t", r.bodies[0], h.Get("X-Naozhi-Signature")) || Verify("other", r.bodies[0], h.Get("X-Naozhi-Signature")) {
		t.Fatalf("signature %q does not verify", h.Get("X-Naozhi-Signature"))
	}
}

func TestDeliver_FiltersByEventAndSubsystem(t *testing.T) {
	r := &recv{}
	srv := httptest.NewServer(http.HandlerFunc(r.handler))
	defer srv.Close()
	s := New([]Endpoint{{URL: srv.URL, Events: []string{EventRunEnded}, Subsystems: []string{"cron"}}}, WithSleep(noSleep))
	s.Deliver(Event{Type: EventRunStarted, Subsystem: "cron", RunID: "a"})
	s.Deliver(Event{Type: EventRunEnded, Subsystem: "sysession", RunID: "b"})
	s.Deliver(Event{Type: EventRunEnded, Subsystem: "cron", RunID: "c"})
	s.Close(context.Background())
	if n := r.hits.Load(); n != 1 {
		t.Fatalf("hits = %d, want only the cron run.ended", n)
	}
}

func TestSend_RetriesOn5xxNotOn4xx(t *testing.T) {
	r := &recv{}
	r.status.Store(503) // first hit 503, then 200
	srv := httptest.NewServer(http.HandlerFunc(r.handler))
	defer srv.Close()
	s := New([]Endpoint{{URL: srv.URL}}, WithSleep(noSleep))
	s.Deliver(Event{Type: EventRunEnded, Subsystem: "cron", RunID: "r"})
	s.Close(context.Background())
	if n := r.hits.Load(); n != 2 {
		t.Fatalf("hits = %d, want a retry after 503", n)
	}

	r2 := &recv{}
	r2.status.Store(400)
	srv2 := httptest.NewServer(http.HandlerFunc(r2.handler))
	defer srv2.Close()
	s2 := New([]Endpoint{{URL: srv2.URL}}, WithSleep(noSleep))
	s2.Deliver(Event{Type: EventRunEnded, Subsystem: "cron", RunID: "r"})
	s2.Close(context.Background())
	if n := r2.hits.Load(); n != 1 {
		t.Fatalf("hits = %d, 4xx must not retry", n)
	}
}

func TestDeliver_NeverBlocksAndSlowEndpointIsIsolated(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { <-release }))
	defer slow.Close()
	fast := &recv{}
	fastSrv := httptest.NewServer(http.HandlerFunc(fast.handler))
	defer fastSrv.Close()
	s := New([]Endpoint{{URL: slow.URL}, {URL: fastSrv.URL}}, WithSleep(noSleep))

	done := make(chan struct{})
	go func() {
		for i := 0; i < QueueDepth+10; i++ {
			s.Deliver(Event{Type: EventRunEnded, Subsystem: "cron", RunID: "r"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Deliver blocked on a full queue")
	}
	// The slow endpoint holds its first event forever, so its queue fills and
	// the overflow is dropped; the fast endpoint keeps receiving meanwhile.
	// (Its own queue may also overflow briefly, so assert a floor, not equality.)
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Close(ctx)
	if n := fast.hits.Load(); n < int32(QueueDepth) {
		t.Fatalf("fast endpoint received %d, want at least %d", n, QueueDepth)
	}
	if dropped := droppedTotal.Get("0"); dropped == nil || dropped.String() == "0" {
		t.Fatal("slow endpoint's overflow should be counted as dropped")
	}
}

func TestClose_AbandonsInFlightOnCtxEnd(t *testing.T) {
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { <-hold }))
	defer func() { close(hold); srv.Close() }()
	s := New([]Endpoint{{URL: srv.URL}}, WithSleep(noSleep))
	s.Deliver(Event{Type: EventRunEnded, Subsystem: "cron", RunID: "r"})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	s.Close(ctx)
	if time.Since(start) > 3*time.Second {
		t.Fatal("Close did not return promptly after ctx ended")
	}
}

func TestNilAndEmptySenderAreSafe(t *testing.T) {
	var s *Sender
	s.Deliver(Event{})
	s.Close(context.Background())
	e := New(nil)
	e.Deliver(Event{Type: EventRunEnded})
	e.Close(context.Background())
	if e.Endpoints() != 0 {
		t.Fatal("endpoints")
	}
}
