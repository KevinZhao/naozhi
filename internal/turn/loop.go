package turn

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/ctxutil"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// receiver is one sink's share of a turn.
type receiver struct {
	origin Origin
	info   TurnInfo
	begun  bool     // Begin has been called
	d      Delivery // Begin's result; nil receives nothing
	done   bool     // Finish has been called
}

// inflight is what the turn being run has reached, read by the panic
// recovery to tell each receiver that has not been finished.
type inflight struct {
	batch     []Msg
	first     bool
	receivers []*receiver
	out       Outcome // the outcome so far; recovery delivers it with Panic set
	afterTurn bool    // Send returned and Sender.AfterTurn has not run yet
	// ctx is the turn's ctx (startTurn), so the panic recovery logs and
	// delivers under the turn's ids; nil before the turn starts.
	ctx context.Context
}

// receiversFor groups batch by Sink in first-appearance order: a group's
// first member is its head and the rest are its Mates. owner joins as an
// Observer when no batch member shares its sink or its Scope (#3004 分叉
// 19a); otherwise the receiver on owner's sink, else the first on its
// scope, is the Primary. Requests with a nil Origin receive nothing.
func receiversFor(owner Origin, batch []Msg, first bool) []*receiver {
	var out []*receiver
	bySink := make(map[string]*receiver, len(batch))
	for _, m := range batch {
		if m.Origin == nil {
			continue
		}
		sink := m.Origin.Sink()
		if r := bySink[sink]; r != nil {
			r.info.Mates = append(r.info.Mates, m.Origin)
			continue
		}
		r := &receiver{origin: m.Origin, info: TurnInfo{Role: RoleHead, First: first, Merged: len(batch)}}
		bySink[sink] = r
		out = append(out, r)
	}
	if owner == nil {
		return out
	}
	r := bySink[owner.Sink()]
	if r == nil {
		r = sameScope(owner, out)
	}
	if r != nil {
		r.info.Primary = true
	} else {
		out = append(out, &receiver{origin: owner, info: TurnInfo{Role: RoleObserver, Merged: len(batch), Primary: true}})
	}
	return out
}

// sameScope is the first of rs whose origin shares owner's Scope; nil when
// owner is not Scoped or its Scope is empty.
func sameScope(owner Origin, rs []*receiver) *receiver {
	o, ok := owner.(Scoped)
	if !ok {
		return nil
	}
	scope := o.Scope()
	if scope == "" {
		return nil
	}
	for _, r := range rs {
		if s, ok := r.origin.(Scoped); ok && s.Scope() == scope {
			return r
		}
	}
	return nil
}

func sessionOpts(owner Origin, key string) sessionview.AgentOpts {
	if owner == nil {
		return sessionview.AgentOpts{}
	}
	return owner.SessionOpts(key)
}

// ownerLoop runs the owner's first turn, then after each turn waits the
// collect delay and drains the queue into one merged turn, until the queue
// is empty or its entry is discarded, removed or recreated (gen no longer
// matches). Its one recover tells the
// in-flight turn's receivers and the queued origins, and NotifyIdle runs
// only after that: the session must not read idle mid-recovery.
func (o *Orchestrator) ownerLoop(ctx context.Context, key string, gen uint64, first Msg) {
	defer o.s.NotifyIdle()
	owner := first.Origin
	cur := &inflight{batch: []Msg{first}, first: true}
	defer func() {
		if r := recover(); r != nil {
			o.recovered(ctx, key, owner, cur, r, gen)
		}
	}()

	o.runBatch(ctx, key, owner, cur)

	timer := time.NewTimer(o.q.CollectDelay())
	defer timer.Stop()
	for {
		cur = nil
		select {
		case <-ctx.Done():
			o.dropOwned(context.WithoutCancel(ctx), key, gen, DropShutdown)
			return
		case <-timer.C:
		}
		batch := o.q.DoneOrDrain(key, gen)
		if batch == nil {
			return
		}
		cur = &inflight{batch: batch}
		o.runBatch(ctx, key, owner, cur)
		// The case arm above consumed the channel, so Reset needs no Stop+drain.
		timer.Reset(o.q.CollectDelay())
	}
}

// runBatch runs one turn of the owner loop over t.batch.
func (o *Orchestrator) runBatch(ctx context.Context, key string, owner Origin, t *inflight) {
	ctx = startTurn(ctx, key, t)
	t.receivers = receiversFor(owner, t.batch, t.first)
	text, images := t.batch[0].Text, t.batch[0].Images
	if !t.first {
		text, images = Coalesce(t.batch)
		slog.InfoContext(ctx, "turn: processing queued messages", "key", key, "count", len(t.batch), "merged_len", len(text))
	}
	o.runTurn(ctx, key, t, sessionOpts(owner, key), text, images, SendSpec{})
}

// runTurn is one turn, shared by the owner loop and detached turns:
// Begin and BeforeSession on every receiver, GetOrCreate, SessionReady,
// Send, then the outcome to every receiver.
func (o *Orchestrator) runTurn(ctx context.Context, key string, t *inflight, opts sessionview.AgentOpts, text string, images []clievent.Attachment, spec SendSpec) {
	for _, r := range t.receivers {
		r.begun = true
		r.d = r.origin.Begin(ctx, r.info)
	}
	for _, r := range t.receivers {
		if r.d != nil {
			r.d.BeforeSession(ctx)
		}
	}
	sess, st, err := o.s.GetOrCreate(ctx, key, opts)
	if err != nil {
		t.out.Err = err
		o.deliver(ctx, key, t, t.out, false)
		return
	}
	t.out = Outcome{Stage: StageSend, Sess: sess}
	var callbacks []clievent.EventCallback
	for _, r := range t.receivers {
		if r.d == nil {
			continue
		}
		if cb := r.d.SessionReady(ctx, st); cb != nil {
			callbacks = append(callbacks, cb)
		}
	}
	result, err := o.s.Send(ctx, key, sess, text, images, spec, joinCallbacks(callbacks))
	t.out.Err, t.out.Result, t.afterTurn = err, result, true
	if err == nil {
		t.out.Stage = StageDone
	}
	o.deliver(ctx, key, t, t.out, false)
}

// deliver finishes the non-blocking receivers not yet finished, then (if it
// is still owed) the Sender's AfterTurn broadcast, then the blocking
// receivers (#3004 分叉 21). recovering guards each call, since a panic here
// would escape the recover.
func (o *Orchestrator) deliver(ctx context.Context, key string, t *inflight, out Outcome, recovering bool) {
	call := func(hook string, fn func()) {
		if recovering {
			guarded(key, hook, fn)
		} else {
			fn()
		}
	}
	finish := func(blocking bool) {
		for _, r := range t.receivers {
			if r.d == nil || r.done {
				continue
			}
			call("Finish", func() {
				// done goes up first so a receiver that panics is not called again.
				r.done = true
				if r.d.Blocking() != blocking {
					r.done = false
					return
				}
				r.d.Finish(ctx, out)
			})
		}
	}
	finish(false)
	if t.afterTurn {
		t.afterTurn = false
		call("AfterTurn", func() { o.s.AfterTurn(key) })
	}
	finish(true)
}

func joinCallbacks(cbs []clievent.EventCallback) clievent.EventCallback {
	switch len(cbs) {
	case 0:
		return nil
	case 1:
		return cbs[0]
	}
	return func(ev clievent.Event) {
		for _, cb := range cbs {
			cb(ev)
		}
	}
}

// recovered handles a panic in a turn on key: it counts and logs it,
// discards the queue (each dropped origin sees DropPanic; an owner's gen
// must still match, so a stale owner leaves a later one's queue alone, while
// a detached turn passes detachedGen and always discards), then delivers the
// in-flight turn t's outcome so far (nil between turns), with Panic set, to
// every receiver not yet finished, running a still-owed AfterTurn in its
// usual place. A receiver whose Begin was never reached is begun here; one
// whose Begin or Finish panicked is not called again.
func (o *Orchestrator) recovered(ctx context.Context, key string, owner Origin, t *inflight, r any, gen uint64) {
	metrics.PanicRecoveredTotal.Add(1)
	if t != nil && t.ctx != nil {
		ctx = t.ctx
	}
	slog.ErrorContext(ctx, "turn: panic recovered", "key", key, "panic", r, "stack", string(debug.Stack()))
	// The turn's ctx may already be Done (shutdown racing the panic).
	ctx = context.WithoutCancel(ctx)
	if gen == detachedGen {
		o.dropQueued(ctx, key, DropPanic)
	} else {
		o.dropOwned(ctx, key, gen, DropPanic)
	}
	if t == nil {
		return
	}
	if t.receivers == nil {
		guarded(key, "Sink", func() { t.receivers = receiversFor(owner, t.batch, t.first) })
	}
	for _, rc := range t.receivers {
		if !rc.begun {
			rc.begun = true
			guarded(key, "Begin", func() { rc.d = rc.origin.Begin(ctx, rc.info) })
		}
	}
	out := t.out
	out.Panic = true
	o.deliver(ctx, key, t, out, true)
}

// maxLoggedTraces caps the trace ids "turn: start" lists for a merged batch.
const maxLoggedTraces = 8

// startTurn derives the turn's ctx from the loop's: the head message's
// trace id (a later batch must not inherit the first message's), a fresh
// run id the session's run record adopts, and the session key. It logs one
// "turn: start" line naming every merged message's trace.
func startTurn(ctx context.Context, key string, t *inflight) context.Context {
	traces := make([]string, 0, min(len(t.batch), maxLoggedTraces))
	for _, m := range t.batch[:min(len(t.batch), maxLoggedTraces)] {
		traces = append(traces, m.TraceID)
	}
	if len(t.batch) > 0 {
		ctx = ctxutil.WithTraceID(ctx, t.batch[0].TraceID)
	}
	ctx = ctxutil.WithSessionKey(ctxutil.WithRunID(ctx, ctxutil.NewTraceID()), key)
	t.ctx = ctx
	slog.InfoContext(ctx, "turn: start", "key", key, "batch", len(t.batch), "trace_ids", traces)
	return ctx
}
