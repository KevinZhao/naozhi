// Package webhook delivers run lifecycle events to HTTP endpoints
// (docs/rfc/outbound-webhooks.md). Leaf: stdlib only. Deliver never blocks
// the caller; each endpoint has its own goroutine and bounded queue, so one
// slow receiver cannot delay another or the producer.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/promexport"
)

// Event types an endpoint may subscribe to.
const (
	EventRunStarted = "run.started"
	EventRunEnded   = "run.ended"
)

const (
	// QueueDepth is the per-endpoint buffer; past it Deliver drops and counts.
	QueueDepth = 256
	// DefaultTimeout bounds one HTTP attempt.
	DefaultTimeout = 10 * time.Second
	// maxAttempts is the first try plus retries; backoff doubles from firstBackoff.
	maxAttempts  = 4
	firstBackoff = time.Second
	userAgent    = "naozhi-webhook/1"
)

var (
	deliveredTotal = promexport.NewMap("naozhi_webhook_delivered_total", "endpoint")
	failedTotal    = promexport.NewMap("naozhi_webhook_failed_total", "endpoint")
	droppedTotal   = promexport.NewMap("naozhi_webhook_dropped_total", "endpoint")
)

// Endpoint is one configured receiver. Empty Events / Subsystems mean all.
type Endpoint struct {
	URL        string
	Secret     string
	Events     []string
	Subsystems []string
	Timeout    time.Duration
}

// Event is the payload. It carries run metadata only: no prompt, result or
// error text (see the RFC's non-goals).
type Event struct {
	Type       string    `json:"type"`
	Subsystem  string    `json:"subsystem"`
	OwnerID    string    `json:"owner_id"`
	RunID      string    `json:"run_id"`
	State      string    `json:"state,omitempty"`
	Trigger    string    `json:"trigger,omitempty"`
	ErrorClass string    `json:"error_class,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	Node       string    `json:"node,omitempty"`
}

// Sender fans events out to its endpoints.
type Sender struct {
	workers []*worker
	wg      sync.WaitGroup
	sleep   func(context.Context, time.Duration)
	node    string
}

// Option tunes a Sender.
type Option func(*Sender)

// WithClient replaces the HTTP client every endpoint uses (tests).
func WithClient(c *http.Client) Option {
	return func(s *Sender) {
		for _, w := range s.workers {
			w.client = c
		}
	}
}

// WithNode sets Event.Node, the sending naozhi's workspace id.
func WithNode(id string) Option { return func(s *Sender) { s.node = id } }

// Node is the workspace id stamped on every event.
func (s *Sender) Node() string { return s.node }

// WithSleep replaces the retry backoff sleep (tests).
func WithSleep(fn func(context.Context, time.Duration)) Option {
	return func(s *Sender) { s.sleep = fn }
}

// New starts one worker per endpoint. A Sender with no endpoints is valid
// and drops everything silently.
func New(eps []Endpoint, opts ...Option) *Sender {
	s := &Sender{sleep: func(ctx context.Context, d time.Duration) {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
		case <-t.C:
		}
	}}
	for i, ep := range eps {
		timeout := ep.Timeout
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		s.workers = append(s.workers, &worker{
			idx:    strconv.Itoa(i),
			ep:     ep,
			host:   hostOf(ep.URL),
			queue:  make(chan Event, QueueDepth),
			client: &http.Client{Timeout: timeout},
			s:      s,
		})
	}
	for _, o := range opts {
		o(s)
	}
	for _, w := range s.workers {
		s.wg.Add(1)
		go w.run()
	}
	return s
}

// Endpoints reports how many receivers are configured.
func (s *Sender) Endpoints() int { return len(s.workers) }

// Deliver enqueues ev for every endpoint subscribed to its type and
// subsystem. Never blocks: a full queue drops the event and counts it.
func (s *Sender) Deliver(ev Event) {
	if s == nil {
		return
	}
	for _, w := range s.workers {
		if !w.wants(ev) {
			continue
		}
		select {
		case w.queue <- ev:
		default:
			droppedTotal.Add(w.idx, 1)
			slog.Warn("webhook queue full; event dropped", "endpoint", w.idx, "host", w.host, "type", ev.Type)
		}
	}
}

// Close stops accepting events and waits for the queues to drain or ctx to
// end, whichever first. Attempts in flight when ctx ends are abandoned.
func (s *Sender) Close(ctx context.Context) {
	if s == nil {
		return
	}
	for _, w := range s.workers {
		close(w.queue)
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		for _, w := range s.workers {
			w.abort()
		}
		<-done
	}
}

type worker struct {
	idx    string
	ep     Endpoint
	host   string
	queue  chan Event
	client *http.Client
	s      *Sender

	mu     sync.Mutex
	cancel context.CancelFunc // of the attempt in flight
	closed bool
}

func (w *worker) wants(ev Event) bool {
	return contains(w.ep.Events, ev.Type) && contains(w.ep.Subsystems, ev.Subsystem)
}

// contains treats an empty list as "everything".
func contains(list []string, v string) bool {
	if len(list) == 0 {
		return true
	}
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (w *worker) run() {
	defer w.s.wg.Done()
	for ev := range w.queue {
		w.send(ev)
	}
}

// abort cancels the current attempt and every later one.
func (w *worker) abort() {
	w.mu.Lock()
	w.closed = true
	if w.cancel != nil {
		w.cancel()
	}
	w.mu.Unlock()
}

// attemptCtx returns a ctx for one attempt, or false once aborted.
func (w *worker) attemptCtx() (context.Context, context.CancelFunc, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	return ctx, cancel, true
}

// send tries ev until a receiver accepts it, a non-retryable status comes
// back, or the attempts run out.
func (w *worker) send(ev Event) {
	body, err := json.Marshal(ev)
	if err != nil {
		failedTotal.Add(w.idx, 1)
		return
	}
	backoff := firstBackoff
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		ctx, cancel, ok := w.attemptCtx()
		if !ok {
			failedTotal.Add(w.idx, 1)
			return
		}
		status, err := w.post(ctx, ev, body)
		cancel()
		switch {
		case err == nil && status/100 == 2:
			deliveredTotal.Add(w.idx, 1)
			return
		case err == nil && !retryable(status):
			failedTotal.Add(w.idx, 1)
			slog.Warn("webhook rejected", "endpoint", w.idx, "host", w.host, "status", status, "type", ev.Type, "run_id", ev.RunID)
			return
		}
		if attempt == maxAttempts {
			failedTotal.Add(w.idx, 1)
			slog.Warn("webhook delivery failed", "endpoint", w.idx, "host", w.host, "status", status, "err", err, "attempts", attempt, "type", ev.Type, "run_id", ev.RunID)
			return
		}
		w.s.sleep(ctx, backoff)
		backoff *= 2
	}
}

func retryable(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status/100 == 5
}

func (w *worker) post(ctx context.Context, ev Event, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.ep.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Naozhi-Event", ev.Type)
	req.Header.Set("X-Naozhi-Delivery", ev.RunID+"-"+ev.Type)
	if w.ep.Secret != "" {
		req.Header.Set("X-Naozhi-Signature", Sign(w.ep.Secret, body))
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// Sign returns the X-Naozhi-Signature value for body: "sha256=" + hex HMAC.
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// Verify reports whether sig is Sign(secret, body); for receivers and tests.
func Verify(secret string, body []byte, sig string) bool {
	return hmac.Equal([]byte(sig), []byte(Sign(secret, body)))
}

// hostOf is the URL's host for log lines (the full URL may carry a token).
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "?"
	}
	return u.Host
}
