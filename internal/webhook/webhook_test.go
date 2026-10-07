package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

type sleepCall struct {
	d     time.Duration
	alive bool // ctx not yet done when the sleep began
}

func TestSend_BackoffSleepsOnLiveCtx(t *testing.T) {
	r := &recv{}
	r.status.Store(500) // every attempt fails
	srv := httptest.NewServer(http.HandlerFunc(r.handler))
	defer srv.Close()
	var mu sync.Mutex
	var calls []sleepCall
	sleeper := func(ctx context.Context, d time.Duration) {
		mu.Lock()
		calls = append(calls, sleepCall{d: d, alive: ctx.Err() == nil})
		mu.Unlock()
	}
	s := New([]Endpoint{{URL: srv.URL}}, WithSleep(sleeper))
	s.Deliver(Event{Type: EventRunEnded, Subsystem: "cron", RunID: "r"})
	s.Close(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if n := r.hits.Load(); n != maxAttempts {
		t.Fatalf("hits = %d, want %d", n, maxAttempts)
	}
	if len(calls) != maxAttempts-1 {
		t.Fatalf("sleeps = %d, want %d", len(calls), maxAttempts-1)
	}
	base := firstBackoff
	for i, c := range calls {
		if !c.alive {
			t.Errorf("sleep %d got an already-cancelled ctx, so it would not wait", i)
		}
		if c.d < base || c.d >= base+base/4 {
			t.Errorf("sleep %d = %v, want [%v, %v)", i, c.d, base, base+base/4)
		}
		base *= 2
	}
}

func TestSend_RealBackoffWaitsBetweenAttempts(t *testing.T) {
	var mu sync.Mutex
	var at []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	const base = 40 * time.Millisecond
	s := New([]Endpoint{{URL: srv.URL}}, withFirstBackoff(base))
	s.Deliver(Event{Type: EventRunEnded, Subsystem: "cron", RunID: "r"})
	s.Close(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(at) != maxAttempts {
		t.Fatalf("attempts = %d, want %d", len(at), maxAttempts)
	}
	want := base
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < want {
			t.Errorf("gap before attempt %d = %v, want >= %v", i+1, gap, want)
		}
		want *= 2
	}
}

func TestClose_InterruptsBackoffOnCtxEnd(t *testing.T) {
	r := &recv{}
	r.status.Store(500)
	srv := httptest.NewServer(http.HandlerFunc(r.handler))
	defer srv.Close()
	s := New([]Endpoint{{URL: srv.URL}}, withFirstBackoff(time.Hour))
	s.Deliver(Event{Type: EventRunEnded, Subsystem: "cron", RunID: "r"})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	s.Close(ctx)
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("Close took %v; the backoff sleep ignored shutdown", el)
	}
	if n := r.hits.Load(); n != 1 {
		t.Fatalf("hits = %d, want no attempt after the aborted backoff", n)
	}
}

func TestBackoffFor_BoundedAndJittered(t *testing.T) {
	for attempt := 1; attempt <= 40; attempt++ {
		for i := 0; i < 20; i++ {
			d := backoffFor(time.Second, attempt)
			if d < min(time.Second<<min(attempt-1, 10), maxBackoff) || d >= maxBackoff+maxBackoff/4 {
				t.Fatalf("attempt %d: backoff %v out of bounds", attempt, d)
			}
		}
	}
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		seen[backoffFor(time.Second, 1)] = true
	}
	if len(seen) < 2 {
		t.Fatal("backoff carries no jitter")
	}
}

// captureLogs routes the default slog logger into a buffer for the test.
func captureLogs(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func assertNoURLSecrets(t *testing.T, out string, wantTarget string) {
	t.Helper()
	for _, secret := range []string{"SEKRET123", "PATHTOK", "hunter2", "alice", "/hooks"} {
		if strings.Contains(out, secret) {
			t.Errorf("log leaked %q: %s", secret, out)
		}
	}
	if !strings.Contains(out, wantTarget) {
		t.Errorf("log lacks target %q: %s", wantTarget, out)
	}
}

func TestSend_NetworkFailureLogRedactsURL(t *testing.T) {
	logs := captureLogs(t)
	// Port 1 refuses the connection, so err is a *url.Error carrying the URL.
	raw := "http://alice:hunter2@127.0.0.1:1/hooks/PATHTOK?token=SEKRET123"
	s := New([]Endpoint{{URL: raw}}, WithSleep(noSleep))
	s.Deliver(Event{Type: EventRunEnded, Subsystem: "cron", RunID: "r"})
	s.Close(context.Background())
	out := logs.String()
	if !strings.Contains(out, "webhook delivery failed") {
		t.Fatalf("no failure line: %s", out)
	}
	assertNoURLSecrets(t, out, "http://127.0.0.1:1")
	if !strings.Contains(out, "url_id="+urlID(raw)) {
		t.Errorf("log lacks the stable url_id: %s", out)
	}
}

func TestSend_RejectedLogRedactsURL(t *testing.T) {
	logs := captureLogs(t)
	r := &recv{}
	r.status.Store(400)
	srv := httptest.NewServer(http.HandlerFunc(r.handler))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	raw := "http://alice:hunter2@" + u.Host + "/hooks/PATHTOK?token=SEKRET123"
	s := New([]Endpoint{{URL: raw}}, WithSleep(noSleep))
	s.Deliver(Event{Type: EventRunEnded, Subsystem: "cron", RunID: "r"})
	s.Close(context.Background())
	out := logs.String()
	if !strings.Contains(out, "webhook rejected") {
		t.Fatalf("no rejected line: %s", out)
	}
	assertNoURLSecrets(t, out, "http://"+u.Host)
}

func TestRedactErr_ScrubsURLFromAnyError(t *testing.T) {
	raw := "https://alice:hunter2@example.com/hooks/PATHTOK?token=SEKRET123"
	for _, err := range []error{
		&url.Error{Op: "Post", URL: raw, Err: errors.New("boom")},
		&url.Error{Op: "Post", URL: "https://elsewhere.example/redirect?token=SEKRET123", Err: errors.New("boom")},
		fmt.Errorf("wrapped: %w", &url.Error{Op: "Post", URL: raw, Err: errors.New("boom")}),
		fmt.Errorf("plain %s", raw),
	} {
		got := redactErr(err, raw)
		for _, secret := range []string{"SEKRET123", "PATHTOK", "hunter2"} {
			if strings.Contains(got, secret) {
				t.Errorf("redactErr(%v) = %q leaks %q", err, got, secret)
			}
		}
	}
	if redactErr(nil, raw) != "" {
		t.Fatal("nil error should redact to empty")
	}
}

func TestEndpoint_LogValueRedacts(t *testing.T) {
	var buf bytes.Buffer
	ep := Endpoint{URL: "https://alice:hunter2@example.com/hooks/PATHTOK?token=SEKRET123", Secret: "sigkey-xyz", Events: []string{EventRunEnded}}
	slog.New(slog.NewTextHandler(&buf, nil)).Info("ep", "endpoint", ep)
	out := buf.String()
	assertNoURLSecrets(t, out, "https://example.com")
	if strings.Contains(out, "sigkey-xyz") || !strings.Contains(out, "[REDACTED]") {
		t.Errorf("secret not redacted: %s", out)
	}
}

func TestRedactURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://alice:pw@example.com:8443/p?q=1#f": "https://example.com:8443",
		"http://127.0.0.1:9/x":                      "http://127.0.0.1:9",
		"not a url":                                 "?",
		"://bad":                                    "?",
	} {
		if got := RedactURL(raw); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", raw, got, want)
		}
	}
	if urlID("https://a.example/x") == urlID("https://a.example/y") {
		t.Fatal("url_id must tell endpoints on one host apart")
	}
}

func TestDeliverAndCloseAfterCloseAreSafe(t *testing.T) {
	r := &recv{}
	srv := httptest.NewServer(http.HandlerFunc(r.handler))
	defer srv.Close()
	s := New([]Endpoint{{URL: srv.URL}}, WithSleep(noSleep))
	s.Close(context.Background())
	s.Deliver(Event{Type: EventRunEnded, Subsystem: "cron", RunID: "late"})
	s.Close(context.Background())
	if n := r.hits.Load(); n != 0 {
		t.Fatalf("hits = %d; an event delivered after Close must be dropped", n)
	}
}

func TestDeliverConcurrentWithClose(t *testing.T) {
	r := &recv{}
	srv := httptest.NewServer(http.HandlerFunc(r.handler))
	defer srv.Close()
	s := New([]Endpoint{{URL: srv.URL}}, WithSleep(noSleep))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s.Deliver(Event{Type: EventRunEnded, Subsystem: "cron", RunID: "r"})
			}
		}()
	}
	s.Close(context.Background())
	wg.Wait()
}

func withFirstBackoff(d time.Duration) Option { return func(s *Sender) { s.firstBackoff = d } }
