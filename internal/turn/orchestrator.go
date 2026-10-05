package turn

import (
	"context"
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// Orchestrator runs every turn on the keys of its queue: it admits requests,
// owns the drain loop, and delivers each turn's outcome to the origins whose
// requests it carried. The queue is its only state, and nothing outside turn
// reaches it: every entry submits here.
type Orchestrator struct {
	q *queue
	s Sender
}

// New returns an Orchestrator over a queue built from qo, sending through s.
func New(qo QueueOptions, s Sender) *Orchestrator {
	return &Orchestrator{q: newQueue(qo), s: s}
}

// Submit admits r. A PriorityNow request, or any request in ModePassthrough,
// runs as a detached turn; otherwise r is enqueued, and if the key was idle
// the caller's Admission runs the owner loop. r.Origin.Admitted sees the
// returned Ack before any turn of r starts.
func (o *Orchestrator) Submit(ctx context.Context, r Request, a Admission) Ack {
	if r.Priority == PriorityNow || o.q.Mode() == ModePassthrough {
		start, ok := a.Admit(RunDetached)
		if !ok {
			admitted(ctx, r.Origin, AckShuttingDown)
			return AckShuttingDown
		}
		admitted(ctx, r.Origin, AckDetached)
		start(func(ctx context.Context) { o.runDetached(ctx, r) })
		return AckDetached
	}

	m := Msg{Text: r.Text, Images: r.Images, EnqueueAt: time.Now(), Origin: r.Origin}
	res := o.q.Enqueue(r.Key, m)
	if !res.isOwner {
		if res.evicted {
			dropped(ctx, r.Key, res.dropped, DropEvicted)
		}
		if res.shouldInterrupt {
			o.interrupt(r.Key)
		}
		ack := AckQueued
		if !res.enqueued {
			ack = AckDropped
		}
		admitted(ctx, r.Origin, ack)
		return ack
	}

	start, ok := a.Admit(RunOwner)
	if !ok {
		// Release ownership so a later request can own the key; anything
		// queued behind it since was acked AckQueued and is told.
		o.dropQueued(ctx, r.Key, DropShutdown)
		admitted(ctx, r.Origin, AckShuttingDown)
		return AckShuttingDown
	}
	admitted(ctx, r.Origin, AckOwner)
	start(func(ctx context.Context) { o.ownerLoop(ctx, r.Key, res.gen, m) })
	return AckOwner
}

func admitted(ctx context.Context, origin Origin, a Ack) {
	if origin != nil {
		origin.Admitted(ctx, a)
	}
}

// interrupt aborts the in-flight turn for ModeInterrupt's first follow-up.
// Every outcome but Sent leaves the follow-up to the next drain (collect).
func (o *Orchestrator) interrupt(key string) {
	switch outcome := o.s.Interrupt(key); outcome {
	case sessionview.InterruptSent:
		slog.Info("turn: interrupt mode aborted the active turn to process a follow-up", "key", key)
	case sessionview.InterruptNoTurn, sessionview.InterruptNoSession, sessionview.InterruptUnsupported:
		slog.Debug("turn: interrupt mode fell back to collect", "key", key, "outcome", outcome.String())
	case sessionview.InterruptError:
		slog.Warn("turn: interrupt mode transport error, falling back to collect", "key", key)
	}
}

// Reset discards key's queue (each dropped origin sees DropReset), fails the
// session's in-flight passthrough sends with clierr.ErrSessionReset, then
// resets the session. The discard comes first: the session reset retires the
// key, and Retire would tell the dropped origins DropRemoved instead (#2185).
func (o *Orchestrator) Reset(ctx context.Context, key string, discardOverride bool) {
	o.dropQueued(ctx, key, DropReset)
	o.s.DiscardPending(key, clierr.ErrSessionReset)
	o.s.Reset(key, discardOverride)
}

// ShouldNotify reports whether key's 3s "message received" cooldown has
// elapsed (queue.ShouldNotify).
func (o *Orchestrator) ShouldNotify(key string) bool {
	return o.q.ShouldNotify(key)
}

// retireNotifyTimeout bounds the DropRemoved notifications of one Retire.
const retireNotifyTimeout = 10 * time.Second

// Retire forgets key's queue state for a key the router retired; an owner
// still running on key stops at its next drain (queue.Cleanup). Each queued
// origin is told DropRemoved on its own goroutine, so the router's Remove
// never waits on an origin's network call; ctx contributes values only.
func (o *Orchestrator) Retire(ctx context.Context, key string) {
	msgs := o.q.Cleanup(key)
	if len(msgs) == 0 {
		return
	}
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), retireNotifyTimeout)
	go func() {
		defer cancel()
		tellDropped(nctx, key, msgs, DropRemoved)
	}()
}

// dropQueued discards key's queue and tells each dropped origin why.
func (o *Orchestrator) dropQueued(ctx context.Context, key string, why DropReason) {
	tellDropped(ctx, key, o.q.DiscardAndReturn(key), why)
}

// dropOwned is dropQueued for the owner holding gen; it leaves a later
// owner's queue alone (queue.DiscardOwned).
func (o *Orchestrator) dropOwned(ctx context.Context, key string, gen uint64, why DropReason) {
	tellDropped(ctx, key, o.q.DiscardOwned(key, gen), why)
}

func tellDropped(ctx context.Context, key string, msgs []Msg, why DropReason) {
	for _, m := range msgs {
		dropped(ctx, key, m, why)
	}
}

// dropped tells m's origin it will never get a turn. A panicking Dropped is
// recovered so the origins after it in a discarded batch are still told.
func dropped(ctx context.Context, key string, m Msg, why DropReason) {
	if m.Origin != nil {
		guarded(key, "Dropped", func() { m.Origin.Dropped(ctx, why) })
	}
}

// guarded runs fn, counting and logging a panic instead of propagating it.
func guarded(key, hook string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			metrics.PanicRecoveredTotal.Add(1)
			slog.Error("turn: panic in "+hook, "key", key, "panic", r)
		}
	}()
	fn()
}
