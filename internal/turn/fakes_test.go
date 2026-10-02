package turn

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// waitTimeout bounds every wait on the recorder; a correct run is far faster.
const waitTimeout = 5 * time.Second

// recorder is the ordered event log every fake writes to, so a test can
// assert on the interleaving of Origin, Delivery and Sender calls.
type recorder struct {
	mu      sync.Mutex
	events  []string
	changed chan struct{}
}

func newRecorder() *recorder { return &recorder{changed: make(chan struct{})} }

func (r *recorder) add(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fmt.Sprintf(format, args...))
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

// count returns how many recorded events equal ev.
func (r *recorder) count(ev string) int {
	n := 0
	for _, e := range r.snapshot() {
		if e == ev {
			n++
		}
	}
	return n
}

// waitFor blocks until ev has been recorded n times.
func (r *recorder) waitFor(t *testing.T, ev string, n int) {
	t.Helper()
	deadline := time.After(waitTimeout)
	for {
		r.mu.Lock()
		got := 0
		for _, e := range r.events {
			if e == ev {
				got++
			}
		}
		ch := r.changed
		r.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("timed out waiting for %q x%d; events: %v", ev, n, r.snapshot())
		}
	}
}

// assertOrder fails unless want appears in the log as a subsequence.
func (r *recorder) assertOrder(t *testing.T, want ...string) {
	t.Helper()
	got := r.snapshot()
	i := 0
	for _, e := range got {
		if i < len(want) && e == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Fatalf("events out of order: want subsequence %v (matched %d), got %v", want, i, got)
	}
}

// fakeOrigin is an Origin named name; its deliveries record "<hook>:<name>".
type fakeOrigin struct {
	name     string
	sink     string
	blocking bool
	rec      *recorder
	opts     sessionview.AgentOpts
	// nilDelivery makes Begin return nil (a receiver that wants nothing).
	nilDelivery bool
	// panicIn names a hook ("begin", "finish", "dropped") that panics.
	panicIn string
	// onEvent, when set, is the callback SessionReady returns.
	onEvent clievent.EventCallback

	mu       sync.Mutex
	infos    []TurnInfo
	outcomes []Outcome
	// doneCtx lists the Finish/Dropped calls that got an already-Done ctx.
	doneCtx []string
}

func (o *fakeOrigin) noteCtx(ctx context.Context, hook string) {
	if ctx.Err() != nil {
		o.mu.Lock()
		o.doneCtx = append(o.doneCtx, hook)
		o.mu.Unlock()
	}
}

func (o *fakeOrigin) doneCtxCalls() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.doneCtx)
}

func (o *fakeOrigin) Sink() string { return o.sink }

func (o *fakeOrigin) Admitted(_ context.Context, a Ack) {
	o.rec.add("admitted:%s:%s", o.name, ackName(a))
}

func (o *fakeOrigin) SessionOpts(string) sessionview.AgentOpts {
	o.rec.add("opts:%s", o.name)
	return o.opts
}

func (o *fakeOrigin) Begin(_ context.Context, t TurnInfo) Delivery {
	o.mu.Lock()
	o.infos = append(o.infos, t)
	o.mu.Unlock()
	o.rec.add("begin:%s:%s", o.name, roleName(t.Role))
	if o.panicIn == "begin" {
		panic("begin " + o.name)
	}
	if o.nilDelivery {
		return nil
	}
	return &fakeDelivery{o: o}
}

func (o *fakeOrigin) Dropped(ctx context.Context, why DropReason) {
	o.noteCtx(ctx, "dropped")
	o.rec.add("dropped:%s:%s", o.name, dropName(why))
	if o.panicIn == "dropped" {
		panic("dropped " + o.name)
	}
}

func (o *fakeOrigin) turnInfos() []TurnInfo {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.infos)
}

func (o *fakeOrigin) finished() []Outcome {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.outcomes)
}

type fakeDelivery struct{ o *fakeOrigin }

func (d *fakeDelivery) BeforeSession(context.Context) { d.o.rec.add("before:%s", d.o.name) }

func (d *fakeDelivery) SessionReady(_ context.Context, st sessionview.SessionStatus) clievent.EventCallback {
	d.o.rec.add("ready:%s:%d", d.o.name, st)
	return d.o.onEvent
}

func (d *fakeDelivery) Finish(ctx context.Context, out Outcome) {
	d.o.noteCtx(ctx, "finish")
	d.o.mu.Lock()
	d.o.outcomes = append(d.o.outcomes, out)
	d.o.mu.Unlock()
	d.o.rec.add("finish:%s:%s", d.o.name, outcomeName(out))
	if d.o.panicIn == "finish" {
		panic("finish " + d.o.name)
	}
}

func (d *fakeDelivery) Blocking() bool { return d.o.blocking }

func newOrigin(rec *recorder, name, sink string) *fakeOrigin {
	return &fakeOrigin{name: name, sink: sink, rec: rec}
}

type fakeSession struct{}

func (fakeSession) Backend() string { return "fake" }

// sendCall is one Sender.Send invocation.
type sendCall struct {
	text    string
	spec    SendSpec
	onEvent clievent.EventCallback
}

// fakeSender records every call. Send blocks on gate when it is set, so a
// test can hold a turn open; sendErr / getErr / panicIn shape the outcome.
type fakeSender struct {
	rec      *recorder
	gate     chan struct{}
	getErr   error
	sendErr  error
	panicGet bool
	panicIf  func(text string) bool // Send panics, once let through the gate, when true
	// panicEarly is panicIf checked before the gate.
	panicEarly func(text string) bool
	status     sessionview.SessionStatus
	interrupt  sessionview.InterruptOutcome

	mu    sync.Mutex
	sends []sendCall
	opts  []sessionview.AgentOpts
}

func newSender(rec *recorder) *fakeSender {
	return &fakeSender{rec: rec}
}

func (s *fakeSender) GetOrCreate(_ context.Context, key string, o sessionview.AgentOpts) (Session, sessionview.SessionStatus, error) {
	s.mu.Lock()
	s.opts = append(s.opts, o)
	s.mu.Unlock()
	s.rec.add("get:%s", key)
	if s.panicGet {
		panic("get")
	}
	if s.getErr != nil {
		return nil, 0, s.getErr
	}
	return fakeSession{}, s.status, nil
}

func (s *fakeSender) Send(_ context.Context, key string, _ Session, text string, _ []clievent.Attachment, spec SendSpec, onEvent clievent.EventCallback) (*clievent.SendResult, error) {
	s.mu.Lock()
	s.sends = append(s.sends, sendCall{text: text, spec: spec, onEvent: onEvent})
	gate := s.gate
	s.mu.Unlock()
	s.rec.add("send:%s:%s", key, text)
	if s.panicEarly != nil && s.panicEarly(text) {
		panic("send")
	}
	if gate != nil {
		<-gate
	}
	if s.panicIf != nil && s.panicIf(text) {
		panic("send")
	}
	if onEvent != nil {
		onEvent(clievent.Event{Type: "assistant"})
	}
	if s.sendErr != nil {
		return nil, s.sendErr
	}
	return &clievent.SendResult{Text: "re:" + text}, nil
}

func (s *fakeSender) AfterTurn(key string) { s.rec.add("after:%s", key) }

func (s *fakeSender) Interrupt(key string) sessionview.InterruptOutcome {
	s.rec.add("interrupt:%s", key)
	return s.interrupt
}

func (s *fakeSender) DiscardPending(key string, reason error) {
	s.rec.add("discardPending:%s:%v", key, reason)
}

func (s *fakeSender) Reset(key string, discardOverride bool) {
	s.rec.add("reset:%s:%v", key, discardOverride)
}

func (s *fakeSender) NotifyIdle() { s.rec.add("idle") }

func (s *fakeSender) sendCalls() []sendCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sends)
}

func (s *fakeSender) texts() []string {
	var out []string
	for _, c := range s.sendCalls() {
		out = append(out, c.text)
	}
	return out
}

// fakeAdmission runs turns inline (IM's owner) or on a goroutine (the
// dashboard's), or declines every Admit.
type fakeAdmission struct {
	rec     *recorder
	async   bool
	decline bool
	ctx     context.Context
	// onAdmit runs inside Admit before the decision.
	onAdmit func()
	wg      sync.WaitGroup
}

func (a *fakeAdmission) Admit(kind RunKind) (func(fn func(ctx context.Context)), bool) {
	a.rec.add("admit:%s", runKindName(kind))
	if a.onAdmit != nil {
		a.onAdmit()
	}
	if a.decline {
		return nil, false
	}
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return func(fn func(ctx context.Context)) {
		a.rec.add("start:%s", runKindName(kind))
		if !a.async {
			fn(ctx)
			return
		}
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			fn(ctx)
		}()
	}, true
}

func ackName(a Ack) string {
	return [...]string{"owner", "queued", "detached", "dropped", "shutting_down"}[a]
}

func roleName(r Role) string { return [...]string{"head", "observer"}[r] }

func dropName(d DropReason) string {
	return [...]string{"reset", "shutdown", "panic", "evicted"}[d]
}

func runKindName(k RunKind) string { return [...]string{"owner", "detached"}[k] }

func outcomeName(o Outcome) string {
	s := [...]string{"session", "send", "done"}[o.Stage]
	if o.Panic {
		s += "+panic"
	}
	return s
}

var errBoom = errors.New("boom")

// harness is one Orchestrator over a real Queue with recording fakes.
type harness struct {
	rec *recorder
	q   *Queue
	s   *fakeSender
	o   *Orchestrator
}

func newHarness(maxDepth int, mode Mode) *harness {
	rec := newRecorder()
	q := NewQueueWithMode(maxDepth, time.Millisecond, mode)
	s := newSender(rec)
	return &harness{rec: rec, q: q, s: s, o: New(q, s)}
}

// hold makes every Send block until the returned release is called once per
// turn to let through.
func (h *harness) hold() (release func()) {
	gate := make(chan struct{})
	h.s.mu.Lock()
	h.s.gate = gate
	h.s.mu.Unlock()
	return func() {
		select {
		case gate <- struct{}{}:
		case <-time.After(waitTimeout):
			panic("release: no Send is waiting on the gate")
		}
	}
}

func (h *harness) submit(text string, origin Origin, a Admission) Ack {
	return h.o.Submit(context.Background(), Request{Key: "k", Text: text, Origin: origin}, a)
}

// coalescedBody strips Coalesce's framing so a test can match on the texts.
func coalescedBody(text string) string {
	return strings.TrimPrefix(text, coalescePrefix)
}
