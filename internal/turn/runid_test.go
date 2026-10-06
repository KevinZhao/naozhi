package turn

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/ctxutil"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// turnIDs is the correlation a hook saw on its ctx.
type turnIDs struct{ hook, run, trace, key string }

func idsOf(hook string, ctx context.Context) turnIDs {
	return turnIDs{hook, ctxutil.RunID(ctx), ctxutil.TraceID(ctx), ctxutil.SessionKey(ctx)}
}

// idLog collects the ids every Sender and Delivery hook saw.
type idLog struct {
	mu  sync.Mutex
	ids []turnIDs
}

func (l *idLog) note(hook string, ctx context.Context) {
	l.mu.Lock()
	l.ids = append(l.ids, idsOf(hook, ctx))
	l.mu.Unlock()
}

func (l *idLog) all() []turnIDs {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.ids)
}

type idSender struct {
	*fakeSender
	log *idLog
}

func (s idSender) GetOrCreate(ctx context.Context, key string, o sessionview.AgentOpts) (Session, sessionview.SessionStatus, error) {
	s.log.note("get", ctx)
	return s.fakeSender.GetOrCreate(ctx, key, o)
}

func (s idSender) Send(ctx context.Context, key string, sess Session, text string, imgs []clievent.Attachment, spec SendSpec, cb clievent.EventCallback) (*clievent.SendResult, error) {
	s.log.note("send", ctx)
	return s.fakeSender.Send(ctx, key, sess, text, imgs, spec, cb)
}

type idOrigin struct {
	*fakeOrigin
	log *idLog
}

func (o idOrigin) Begin(ctx context.Context, t TurnInfo) Delivery {
	o.log.note("begin:"+o.name, ctx)
	d := o.fakeOrigin.Begin(ctx, t)
	if d == nil {
		return nil
	}
	return idDelivery{d, o.log, o.name}
}

type idDelivery struct {
	Delivery
	log  *idLog
	name string
}

func (d idDelivery) Finish(ctx context.Context, out Outcome) {
	d.log.note("finish:"+d.name, ctx)
	d.Delivery.Finish(ctx, out)
}

// captureLog swaps the default logger for a ctx-correlating JSON one; the
// test must not be parallel.
func captureLog(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(ctxutil.NewHandler(slog.NewJSONHandler(buf, nil))))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

// lines returns the decoded records whose msg is msg.
func (b *syncBuf) lines(msg string) []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(b.b.Bytes()), []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(line, &m) == nil && m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

// Every turn gets its own run id, the same one on every hook of that turn,
// and the trace of its head message: a merged batch does not inherit the
// owner loop's first trace, and "turn: start" names every merged trace.
func TestTurnIDs_PerTurnRunIDAndHeadTrace(t *testing.T) {
	logs := captureLog(t)
	rec := newRecorder()
	ids := &idLog{}
	fs := newSender(rec)
	o := New(QueueOptions{MaxDepth: 8, CollectDelay: time.Millisecond, Mode: ModeCollect}, idSender{fs, ids})
	gate := make(chan struct{})
	fs.gate = gate
	release := func() { gate <- struct{}{} }
	// The IM owner loop runs on the first message's ctx, trace included.
	adm := &fakeAdmission{rec: rec, async: true, ctx: ctxutil.WithTraceID(context.Background(), "t1")}
	orig := func(name string) Origin { return idOrigin{newOrigin(rec, name, "ws:"+name), ids} }

	o.Submit(context.Background(), Request{Key: "k", Text: "m1", Origin: orig("a"), TraceID: "t1"}, adm)
	rec.waitFor(t, "send:k:m1", 1)
	o.Submit(context.Background(), Request{Key: "k", Text: "m2", Origin: orig("b"), TraceID: "t2"}, adm)
	o.Submit(ctxutil.WithTraceID(context.Background(), "t3"), Request{Key: "k", Text: "m3", Origin: orig("c")}, adm)
	release()
	rec.waitFor(t, "begin:c:head", 1)
	release()
	rec.waitFor(t, "idle", 1)
	adm.wg.Wait()

	byRun := map[string][]turnIDs{}
	var order []string
	for _, id := range ids.all() {
		if len(id.run) != 16 || id.key != "k" {
			t.Fatalf("%s saw run=%q key=%q", id.hook, id.run, id.key)
		}
		if _, ok := byRun[id.run]; !ok {
			order = append(order, id.run)
		}
		byRun[id.run] = append(byRun[id.run], id)
	}
	if len(order) != 2 {
		t.Fatalf("want 2 distinct run ids, got %v", ids.all())
	}
	wantHooks := [][]string{
		{"begin:a", "get", "send", "finish:a"},
		{"begin:b", "begin:c", "begin:a", "get", "send", "finish:b", "finish:c", "finish:a"},
	}
	for i, run := range order {
		var hooks []string
		for _, id := range byRun[run] {
			hooks = append(hooks, id.hook)
			if want := []string{"t1", "t2"}[i]; id.trace != want {
				t.Errorf("turn %d %s trace = %q, want %q", i+1, id.hook, id.trace, want)
			}
		}
		if !slices.Equal(hooks, wantHooks[i]) {
			t.Errorf("turn %d hooks on run %s = %v, want %v", i+1, run, hooks, wantHooks[i])
		}
	}

	starts := logs.lines("turn: start")
	if len(starts) != 2 {
		t.Fatalf("turn: start lines = %v", starts)
	}
	got := starts[1]
	if got["run_id"] != order[1] || got["trace_id"] != "t2" || got["batch"] != float64(2) {
		t.Fatalf("second turn: start = %v", got)
	}
	traces, _ := got["trace_ids"].([]any)
	if len(traces) != 2 || traces[0] != "t2" || traces[1] != "t3" {
		t.Fatalf("trace_ids = %v, want [t2 t3]", got["trace_ids"])
	}
}

// A detached turn with no trace anywhere gets a minted one, and a run id.
func TestTurnIDs_DetachedMintsTrace(t *testing.T) {
	logs := captureLog(t)
	rec := newRecorder()
	ids := &idLog{}
	o := New(QueueOptions{MaxDepth: 8, CollectDelay: time.Millisecond, Mode: ModePassthrough}, idSender{newSender(rec), ids})
	o.Submit(context.Background(), Request{Key: "k", Text: "p", Origin: idOrigin{newOrigin(rec, "a", "ws:a"), ids}}, &fakeAdmission{rec: rec})

	all := ids.all()
	if len(all) != 4 { // begin, get, send, finish
		t.Fatalf("hooks = %v", all)
	}
	for _, id := range all {
		if len(id.trace) != 16 || id.trace != all[0].trace || len(id.run) != 16 || id.run != all[0].run {
			t.Fatalf("detached hooks disagree or lack ids: %v", all)
		}
	}
	if s := logs.lines("turn: start"); len(s) != 1 || s[0]["trace_id"] != all[0].trace {
		t.Fatalf("turn: start = %v", s)
	}
}

// A panicking turn is recovered under its own ids: "turn: panic recovered"
// and the panic-time Finish carry the turn's run id and head trace, not the
// owner loop's ctx (which has the first message's trace and no run id).
func TestTurnIDs_PanicRecoveredOnTheTurnCtx(t *testing.T) {
	logs := captureLog(t)
	rec := newRecorder()
	ids := &idLog{}
	fs := newSender(rec)
	fs.panicIf = func(text string) bool { return text != "m1" }
	o := New(QueueOptions{MaxDepth: 8, CollectDelay: time.Millisecond, Mode: ModeCollect}, idSender{fs, ids})
	gate := make(chan struct{})
	fs.gate = gate
	adm := &fakeAdmission{rec: rec, async: true, ctx: ctxutil.WithTraceID(context.Background(), "t1")}
	orig := func(name string) Origin { return idOrigin{newOrigin(rec, name, "ws:"+name), ids} }

	o.Submit(context.Background(), Request{Key: "k", Text: "m1", Origin: orig("a"), TraceID: "t1"}, adm)
	rec.waitFor(t, "send:k:m1", 1)
	o.Submit(context.Background(), Request{Key: "k", Text: "m2", Origin: orig("b"), TraceID: "t2"}, adm)
	gate <- struct{}{}
	rec.waitFor(t, "send:k:m2", 1)
	gate <- struct{}{}
	rec.waitFor(t, "idle", 1)
	adm.wg.Wait()

	var send, finish turnIDs
	for _, id := range ids.all() {
		if id.trace != "t2" {
			continue
		}
		switch id.hook {
		case "send":
			send = id
		case "finish:b":
			finish = id
		}
	}
	if send.run == "" || finish.run != send.run {
		t.Fatalf("panicking turn: send=%+v finish=%+v (all %v)", send, finish, ids.all())
	}
	p := logs.lines("turn: panic recovered")
	if len(p) != 1 || p[0]["run_id"] != send.run || p[0]["trace_id"] != "t2" || p[0]["session_key"] != "k" {
		t.Fatalf("turn: panic recovered = %v, want run_id %s trace_id t2", p, send.run)
	}
}
