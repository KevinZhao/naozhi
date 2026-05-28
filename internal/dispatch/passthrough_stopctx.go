package dispatch

import (
	"context"
	"log/slog"
	"time"
)

// mergeStopAndValues returns a context whose Deadline / Done / Err signals
// flow from cancelSrc, while Value lookups consult valuesSrc. Used by the
// passthrough send branch to detach the per-webhook ctx (handlers return
// in seconds, LLM turns take minutes) but still let SIGTERM-driven
// graceful shutdown abort the in-flight send before its 5-min internal
// totalTimeout.
//
// Replaces the previous context.WithoutCancel(ctx) call which discarded
// every cancellation source — including the long-lived process-shutdown
// signal — leaving sendAndReply only stoppable by its internal timer.
//
// Caller contract:
//   - cancelSrc should be the long-lived service ctx (typically
//     dispatcher.stopCtx, which NewDispatcher seeds from
//     DispatcherConfig.StopCtx). NewDispatcher's constructor swap
//     guarantees this is always non-nil for production wiring; if a
//     test or future path ever passes nil, we log + degrade to
//     context.Background() rather than panic — the panic was a dead
//     defence (R260528-GO-11).
//   - valuesSrc carries the per-request slog attrs / auth values from
//     the webhook handler. nil valuesSrc treats it as "no values" — Value
//     lookups still consult cancelSrc as a final fallback so attrs the
//     server attached to the service ctx (e.g. cron / sysession trace
//     IDs) remain reachable.
//
// (#1320)
func mergeStopAndValues(cancelSrc, valuesSrc context.Context) context.Context {
	if cancelSrc == nil {
		// Dead defence in normal flow (NewDispatcher seeds stopCtx);
		// degrade gracefully so an unexpected test wiring or future
		// caller bug doesn't crash the dispatcher. R260528-GO-11.
		slog.Error("dispatch: mergeStopAndValues nil cancelSrc, falling back to Background")
		cancelSrc = context.Background()
	}
	if valuesSrc == nil {
		valuesSrc = context.Background()
	}
	return mergedCtx{cancel: cancelSrc, values: valuesSrc}
}

// mergedCtx implements context.Context by composing two parents: cancel
// signals propagate from `cancel`, value lookups consult `values` first
// then fall through to `cancel`. Trivially small wrapper — no goroutines,
// no internal mutex — so the per-message overhead is one allocation.
type mergedCtx struct {
	cancel context.Context
	values context.Context
}

func (c mergedCtx) Deadline() (time.Time, bool) { return c.cancel.Deadline() }
func (c mergedCtx) Done() <-chan struct{}       { return c.cancel.Done() }
func (c mergedCtx) Err() error                  { return c.cancel.Err() }
func (c mergedCtx) Value(key any) any {
	if v := c.values.Value(key); v != nil {
		return v
	}
	return c.cancel.Value(key)
}
