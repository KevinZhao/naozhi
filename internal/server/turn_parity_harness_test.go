package server

// Harness for the turn-parity characterization tests (turn_parity_*_test.go).
// Those tests pin what the IM and dashboard turn pipelines do TODAY for every
// row of #3004's divergence table, so the PRs that merge the two pipelines
// change a row only on purpose: a row that flips edits its assertion in the
// same PR and names the row number.
//
// Everything runs on a real Server from buildServerWithHandlers, so the IM
// dispatcher and the dashboard send engine share the one MessageQueue the
// composition root builds. Sessions are injected TestProcesses whose turns the
// test scripts one by one. Every wait is for an expected event, with
// parityWait as its deadline; no test sleeps to let something happen.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

// parityWait bounds every wait for an expected output. Generous on purpose:
// it only expires when the output never comes.
const parityWait = 5 * time.Second

const (
	parityPlatformName = "parity"
	parityChatID       = "chat1"
	// parityKey is the key the IM chat's general agent resolves to; the
	// dashboard addresses the same key so both entries share one queue slot.
	parityKey     = "parity:direct:chat1:general"
	parityChatKey = "parity:direct:chat1"
)

var errParityBoom = errors.New("parity: scripted turn failure")

// ─── fake IM platform ────────────────────────────────────────────────────────

// parityPlatform records every outbound IM call. It is not a Reactor; wrap it
// in parityReactorPlatform for the ⏳ (ReactionQueued) rows.
type parityPlatform struct {
	mu      sync.Mutex
	replies []string
	edits   []string
	added   []string // message IDs that got ReactionQueued
	removed []string // message IDs whose ReactionQueued was removed
	interim bool
	// onReply runs inside Reply before it records, so a test can observe what
	// had already happened elsewhere at the moment the IM reply went out.
	onReply func(text string)
	replyCh chan string
}

func newParityPlatform(interim bool) *parityPlatform {
	return &parityPlatform{interim: interim, replyCh: make(chan string, 64)}
}

func (p *parityPlatform) Name() string                                           { return parityPlatformName }
func (p *parityPlatform) RegisterRoutes(*http.ServeMux, platform.MessageHandler) {}
func (p *parityPlatform) MaxReplyLength() int                                    { return 4000 }
func (p *parityPlatform) SupportsInterimMessages() bool                          { return p.interim }

func (p *parityPlatform) Reply(_ context.Context, msg platform.OutgoingMessage) (string, error) {
	p.mu.Lock()
	hook := p.onReply
	p.mu.Unlock()
	if hook != nil {
		hook(msg.Text)
	}
	p.mu.Lock()
	p.replies = append(p.replies, msg.Text)
	p.mu.Unlock()
	p.replyCh <- msg.Text
	return "reply-id", nil
}

func (p *parityPlatform) EditMessage(_ context.Context, _ string, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.edits = append(p.edits, text)
	return nil
}

func (p *parityPlatform) setOnReply(fn func(string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onReply = fn
}

// waitReply returns the next IM reply, failing the test if none arrives.
func (p *parityPlatform) waitReply(t *testing.T, what string) string {
	t.Helper()
	select {
	case r := <-p.replyCh:
		return r
	case <-time.After(parityWait):
		t.Fatalf("no IM reply (%s); replies so far: %q", what, p.allReplies())
		return ""
	}
}

func (p *parityPlatform) allReplies() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.replies...)
}

func (p *parityPlatform) allEdits() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.edits...)
}

func (p *parityPlatform) addedFor(id string) int   { return p.count(&p.added, id) }
func (p *parityPlatform) removedFor(id string) int { return p.count(&p.removed, id) }

func (p *parityPlatform) count(list *[]string, id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, v := range *list {
		if v == id {
			n++
		}
	}
	return n
}

type parityReactorPlatform struct{ *parityPlatform }

func (p parityReactorPlatform) AddReaction(_ context.Context, id string, r platform.ReactionType) error {
	if r == platform.ReactionQueued {
		p.mu.Lock()
		p.added = append(p.added, id)
		p.mu.Unlock()
	}
	return nil
}

func (p parityReactorPlatform) RemoveReaction(_ context.Context, id string, r platform.ReactionType) error {
	if r == platform.ReactionQueued {
		p.mu.Lock()
		p.removed = append(p.removed, id)
		p.mu.Unlock()
	}
	return nil
}

// ─── scripted session turns ──────────────────────────────────────────────────

// parityCall is one turn the pipeline handed to the session.
type parityCall struct {
	Text        string
	Priority    string
	Passthrough bool // arrived through SendPassthrough
	OnEvent     clievent.EventCallback
}

// parityOutcome is how the test answers a turn.
type parityOutcome struct {
	Result *clievent.SendResult // nil with nil Err ⇒ {Text: "ok"}
	Err    error
	Panic  any
	Events []clievent.Event // delivered to the turn's callback before returning
}

func okTurn(text string) parityOutcome {
	return parityOutcome{Result: &clievent.SendResult{Text: text}}
}

// parityTurns scripts a TestProcess: every Send / SendPassthrough parks until
// the test answers it, so "the turn is running" is a state the test holds.
type parityTurns struct {
	proc    *session.TestProcess
	calls   chan parityCall
	answers chan parityOutcome
	stop    chan struct{}
	interMu sync.Mutex
	inter   int // InterruptViaControl calls
}

func (tt *parityTurns) run(ctx context.Context, c parityCall) (*clievent.SendResult, error) {
	tt.calls <- c
	var o parityOutcome
	select {
	case o = <-tt.answers:
	case <-tt.stop:
		return nil, errors.New("parity harness stopped")
	}
	if o.Panic != nil {
		panic(o.Panic)
	}
	for _, ev := range o.Events {
		if c.OnEvent != nil {
			c.OnEvent(ev)
		}
	}
	if o.Err != nil {
		return nil, o.Err
	}
	if o.Result == nil {
		return &clievent.SendResult{Text: "ok"}, nil
	}
	return o.Result, nil
}

// next waits for the pipeline to start a turn.
func (tt *parityTurns) next(t *testing.T, what string) parityCall {
	t.Helper()
	select {
	case c := <-tt.calls:
		return c
	case <-time.After(parityWait):
		t.Fatalf("no turn reached the session (%s)", what)
		return parityCall{}
	}
}

func (tt *parityTurns) answer(o parityOutcome) { tt.answers <- o }

// turn waits for the next turn and answers it in one step.
func (tt *parityTurns) turn(t *testing.T, what string, o parityOutcome) parityCall {
	t.Helper()
	c := tt.next(t, what)
	tt.answer(o)
	return c
}

// noMoreTurns asserts nothing else reached the session. Call it only once the
// pipeline is known to be idle (waitEngineIdle, or the IM handler returned).
func (tt *parityTurns) noMoreTurns(t *testing.T) {
	t.Helper()
	select {
	case c := <-tt.calls:
		t.Fatalf("unexpected extra turn reached the session: %+v", c)
	default:
	}
}

func (tt *parityTurns) interrupts() int {
	tt.interMu.Lock()
	defer tt.interMu.Unlock()
	return tt.inter
}

// ─── WebSocket capture client ────────────────────────────────────────────────

type parityFrame struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Status string `json:"status"`
	Key    string `json:"key"`
	State  string `json:"state"`
	Error  string `json:"error"`
}

// parityWS is an authenticated dashboard connection subscribed to parityKey.
// Frames are read straight off the client's send channel.
type parityWS struct {
	h    *parityHarness
	c    *wsClient
	seen []parityFrame
}

// waitFor reads frames until match accepts one.
func (w *parityWS) waitFor(t *testing.T, what string, match func(parityFrame) bool) parityFrame {
	t.Helper()
	deadline := time.After(parityWait)
	for {
		select {
		case data := <-w.c.send:
			var f parityFrame
			_ = json.Unmarshal(data, &f)
			w.seen = append(w.seen, f)
			if match(f) {
				return f
			}
		case <-deadline:
			t.Fatalf("frame never arrived (%s); frames so far: %+v", what, w.seen)
			return parityFrame{}
		}
	}
}

// flush moves every frame already buffered into seen and returns all of them.
func (w *parityWS) flush() []parityFrame {
	for {
		select {
		case data := <-w.c.send:
			var f parityFrame
			_ = json.Unmarshal(data, &f)
			w.seen = append(w.seen, f)
		default:
			return w.seen
		}
	}
}

// sendTo drives Hub.handleSend the way readPump does for a "send" frame on key.
func (w *parityWS) sendTo(key, id, text string) {
	w.h.srv.hub.handleSend(w.c, node.ClientMsg{Type: "send", ID: id, Key: key, Text: text})
}

func (w *parityWS) send(id, text string) { w.sendTo(parityKey, id, text) }

// ack waits for the send_ack of id and returns its status.
func (w *parityWS) ack(t *testing.T, id string) string {
	t.Helper()
	return w.waitFor(t, "send_ack "+id, func(f parityFrame) bool {
		return f.Type == "send_ack" && f.ID == id
	}).Status
}

// errorAcks returns the error send_acks seen so far (after flush).
func (w *parityWS) errorAcks() []parityFrame {
	var out []parityFrame
	for _, f := range w.flush() {
		if f.Type == "send_ack" && f.Status == "error" {
			out = append(out, f)
		}
	}
	return out
}

func (w *parityWS) framesOfType(typ string) []parityFrame {
	var out []parityFrame
	for _, f := range w.flush() {
		if f.Type == typ {
			out = append(out, f)
		}
	}
	return out
}

// ─── harness ─────────────────────────────────────────────────────────────────

type parityOpts struct {
	mode          string // "collect" (default), "interrupt", "passthrough"
	maxDepth      int    // 0 ⇒ 10; use -1 for a disabled queue
	collect       time.Duration
	reactor       bool
	interim       bool
	agents        map[string]session.AgentOpts
	agentCommands map[string]string
}

type parityHarness struct {
	t      *testing.T
	srv    *Server
	hs     *handlerSet
	router *session.Router
	plat   *parityPlatform
	im     platform.MessageHandler
	// imCtx is the inbound ctx every IM message carries; cancelled at cleanup
	// so an owner loop parked in its drain wait cannot outlive the test.
	imCtx    context.Context
	imCancel context.CancelFunc
}

func newParityHarness(t *testing.T, o parityOpts) *parityHarness {
	t.Helper()
	if o.mode == "" {
		o.mode = "collect"
	}
	switch {
	case o.maxDepth == 0:
		o.maxDepth = 10
	case o.maxDepth < 0:
		o.maxDepth = 0
	}
	if o.collect == 0 {
		o.collect = 5 * time.Millisecond
	}
	if o.agents == nil {
		o.agents = map[string]session.AgentOpts{"general": {}}
	}
	plat := newParityPlatform(o.interim)
	var p platform.Platform = plat
	if o.reactor {
		p = parityReactorPlatform{plat}
	}
	router := session.NewRouter(session.RouterConfig{})
	srv, hs := buildServerWithHandlers(ServerOptions{
		Addr:          ":0",
		Router:        router,
		Platforms:     map[string]platform.Platform{parityPlatformName: p},
		Backend:       "claude",
		Agents:        o.agents,
		AgentCommands: o.agentCommands,
		Queue:         QueueOptions{MaxDepth: o.maxDepth, CollectDelay: o.collect, Mode: o.mode},
	})
	h := &parityHarness{t: t, srv: srv, hs: hs, router: router, plat: plat, im: srv.dispatcher.BuildHandler()}
	h.imCtx, h.imCancel = context.WithCancel(context.Background())
	t.Cleanup(func() {
		h.imCancel()
		srv.hub.Shutdown()
		srv.appCancel()
	})
	return h
}

// session injects a scripted session at key. Registered after the server's
// cleanup, so its stop channel closes first and no parked turn can hold
// Shutdown's drain.
func (h *parityHarness) session(key string, passthrough bool) *parityTurns {
	tt := &parityTurns{
		proc:    session.NewTestProcess(),
		calls:   make(chan parityCall, 32),
		answers: make(chan parityOutcome),
		stop:    make(chan struct{}),
	}
	tt.proc.PassthroughVal = passthrough
	tt.proc.SendFunc = func(ctx context.Context, text string, _ []clievent.Attachment, onEvent clievent.EventCallback) (*clievent.SendResult, error) {
		return tt.run(ctx, parityCall{Text: text, OnEvent: onEvent})
	}
	tt.proc.SendPassthroughFunc = func(ctx context.Context, text string, _ []clievent.Attachment, onEvent clievent.EventCallback, priority string) (*clievent.SendResult, error) {
		return tt.run(ctx, parityCall{Text: text, Priority: priority, Passthrough: true, OnEvent: onEvent})
	}
	tt.proc.InterruptViaControlFunc = func() error {
		tt.interMu.Lock()
		defer tt.interMu.Unlock()
		tt.inter++
		return nil
	}
	h.router.InjectSession(key, tt.proc)
	h.t.Cleanup(func() { close(tt.stop) })
	return tt
}

// imMsg builds an inbound IM message from the harness chat; id doubles as the
// platform message ID (the ⏳ target) and, prefixed, the dedup event ID.
func (h *parityHarness) imMsg(id, text string) platform.IncomingMessage {
	return platform.IncomingMessage{
		Platform:  parityPlatformName,
		EventID:   "ev-" + id,
		MessageID: id,
		UserID:    "u1",
		ChatID:    parityChatID,
		ChatType:  "direct",
		Text:      text,
	}
}

// imSend delivers an IM message and returns once the handler does.
func (h *parityHarness) imSend(id, text string) { h.im(h.imCtx, h.imMsg(id, text)) }

// imAsync delivers an IM message on its own goroutine — needed for the owner,
// whose handler runs the whole drain loop — and returns a channel closed when
// the handler returns.
func (h *parityHarness) imAsync(id, text string) <-chan struct{} {
	return h.imAsyncCtx(h.imCtx, id, text)
}

func (h *parityHarness) imAsyncCtx(ctx context.Context, id, text string) <-chan struct{} {
	msg := h.imMsg(id, text)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.im(ctx, msg)
	}()
	return done
}

func (h *parityHarness) waitDone(done <-chan struct{}, what string) {
	h.t.Helper()
	select {
	case <-done:
	case <-time.After(parityWait):
		h.t.Fatalf("IM handler did not return (%s)", what)
	}
}

// ws registers an authenticated dashboard connection subscribed to parityKey.
func (h *parityHarness) ws() *parityWS {
	c := &wsClient{hub: h.srv.hub, send: make(chan []byte, 256), done: make(chan struct{})}
	registerSub(h.srv.hub, c, parityKey)
	return &parityWS{h: h, c: c}
}

// httpSend posts to /api/sessions/send through the real SendHandler and
// returns the response status field.
func (h *parityHarness) httpSend(t *testing.T, text string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"key": parityKey, "text": text})
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.hs.sendH.handleSend(rec, req)
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code >= 400 {
		t.Fatalf("HTTP send %q: %d %s", text, rec.Code, rec.Body.String())
	}
	return resp["status"]
}

func (h *parityHarness) engine() *sendEngine { return h.hs.wiring.engine }

// waitEngineIdle returns once every goroutine the dashboard engine started has
// finished — the owner loop, its drain turns and their error callbacks. Call
// it only when no further dashboard send is in flight.
func (h *parityHarness) waitEngineIdle() {
	h.t.Helper()
	waitEngineIdle(h.t, h.engine())
}

func waitEngineIdle(t *testing.T, e *sendEngine) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(parityWait):
		t.Fatal("dashboard send goroutines still running")
	}
}

// replyMatching reports whether any IM reply contains sub.
func replyMatching(replies []string, sub string) int {
	n := 0
	for _, r := range replies {
		if strings.Contains(r, sub) {
			n++
		}
	}
	return n
}
