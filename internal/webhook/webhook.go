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
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
	// maxAttempts is the first try plus retries; backoff doubles from
	// firstBackoff up to maxBackoff, plus up to 25% jitter.
	maxAttempts  = 4
	firstBackoff = time.Second
	maxBackoff   = 30 * time.Second
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

// LogValue keeps the URL's userinfo, path and query and the signing secret
// out of logs.
func (e Endpoint) LogValue() slog.Value {
	secret := ""
	if e.Secret != "" {
		secret = "[REDACTED]"
	}
	return slog.GroupValue(
		slog.String("url", RedactURL(e.URL)),
		slog.String("url_id", urlID(e.URL)),
		slog.String("secret", secret),
		slog.Any("events", e.Events),
		slog.Any("subsystems", e.Subsystems),
		slog.Duration("timeout", e.Timeout),
	)
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
	workers      []*worker
	wg           sync.WaitGroup
	sleep        func(context.Context, time.Duration)
	firstBackoff time.Duration
	node         string

	mu     sync.RWMutex // guards closed against Deliver's queue sends
	closed bool
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
	s := &Sender{firstBackoff: firstBackoff, sleep: func(ctx context.Context, d time.Duration) {
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
		ctx, stop := context.WithCancel(context.Background())
		s.workers = append(s.workers, &worker{
			idx:    strconv.Itoa(i),
			ep:     ep,
			target: RedactURL(ep.URL),
			urlID:  urlID(ep.URL),
			queue:  make(chan Event, QueueDepth),
			client: &http.Client{Timeout: timeout},
			s:      s,
			ctx:    ctx,
			stop:   stop,
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
// After Close it drops everything.
func (s *Sender) Deliver(ev Event) {
	if s == nil {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
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
			slog.Warn("webhook queue full; event dropped", "endpoint", w.idx, "target", w.target, "url_id", w.urlID, "type", ev.Type)
		}
	}
}

// Close stops accepting events and waits for the queues to drain or ctx to
// end, whichever first. Attempts and backoffs in flight when ctx ends are
// abandoned. Safe to call more than once.
func (s *Sender) Close(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		for _, w := range s.workers {
			close(w.queue)
		}
	}
	s.mu.Unlock()
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
	target string // scheme://host, the only part of the URL that is logged
	urlID  string
	queue  chan Event
	client *http.Client
	s      *Sender

	// ctx parents every attempt and backoff; stop (via abort) ends them all.
	ctx  context.Context
	stop context.CancelFunc
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
	defer w.stop()
	for ev := range w.queue {
		w.send(ev)
	}
}

// abort cancels the current attempt or backoff and every later one.
func (w *worker) abort() { w.stop() }

// send tries ev until a receiver accepts it, a non-retryable status comes
// back, or the attempts run out.
func (w *worker) send(ev Event) {
	body, err := json.Marshal(ev)
	if err != nil {
		failedTotal.Add(w.idx, 1)
		return
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if w.ctx.Err() != nil {
			failedTotal.Add(w.idx, 1)
			return
		}
		ctx, cancel := context.WithCancel(w.ctx)
		status, err := w.post(ctx, ev, body)
		cancel()
		switch {
		case err == nil && status/100 == 2:
			deliveredTotal.Add(w.idx, 1)
			return
		case err == nil && !retryable(status):
			failedTotal.Add(w.idx, 1)
			slog.Warn("webhook rejected", "endpoint", w.idx, "target", w.target, "url_id", w.urlID, "status", status, "type", ev.Type, "run_id", ev.RunID)
			return
		}
		if attempt == maxAttempts {
			failedTotal.Add(w.idx, 1)
			slog.Warn("webhook delivery failed", "endpoint", w.idx, "target", w.target, "url_id", w.urlID, "status", status, "err", redactErr(err, w.ep.URL), "attempts", attempt, "type", ev.Type, "run_id", ev.RunID)
			return
		}
		w.s.sleep(w.ctx, backoffFor(w.s.firstBackoff, attempt))
	}
}

// backoffFor is the wait after the given failed attempt: base doubled per
// attempt, capped at maxBackoff, plus up to 25% random jitter so receivers
// recovering from an outage are not hit in lockstep.
func backoffFor(base time.Duration, attempt int) time.Duration {
	d := base
	for i := 1; i < attempt && d < maxBackoff; i++ {
		d *= 2
	}
	d = min(d, maxBackoff)
	if q := int64(d / 4); q > 0 {
		d += time.Duration(rand.Int64N(q))
	}
	return d
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

// RedactURL is raw reduced to scheme://host, or "?" when unparsable. The
// userinfo, path and query may carry a token, so only this form is logged.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "?"
	}
	return u.Scheme + "://" + u.Host
}

// urlID is a short stable hash of the full URL, telling apart endpoints that
// share a host without revealing their path or query.
func urlID(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:4])
}

// redactErr renders err for a log line without the request URL: a *url.Error
// (whose text embeds the full URL, possibly a redirect target) keeps only its
// op and cause, and any remaining copy of raw is reduced to scheme://host.
func redactErr(err error, raw string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	var uerr *url.Error
	if errors.As(err, &uerr) {
		msg = uerr.Op + " " + RedactURL(uerr.URL)
		if uerr.Err != nil {
			msg += ": " + uerr.Err.Error()
		}
	}
	return strings.ReplaceAll(msg, raw, RedactURL(raw))
}
