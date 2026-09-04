package dispatch

import (
	"context"
	"log/slog"
	"time"
)

// mergeStopAndValues returns a context whose Deadline / Done / Err flow from
// cancelSrc while Value lookups consult valuesSrc (falling back to cancelSrc).
// The passthrough send branch uses it to detach from the per-webhook ctx yet
// still let SIGTERM-driven shutdown abort the in-flight send (#1320).
//
// cancelSrc should be the long-lived service ctx (dispatcher.stopCtx); a nil
// cancelSrc is logged and degraded to context.Background() rather than
// panicking. nil valuesSrc means "no values".
func mergeStopAndValues(cancelSrc, valuesSrc context.Context) context.Context {
	if cancelSrc == nil {
		slog.Error("dispatch: mergeStopAndValues nil cancelSrc, falling back to Background")
		cancelSrc = context.Background()
	}
	if valuesSrc == nil {
		valuesSrc = context.Background()
	}
	return mergedCtx{cancel: cancelSrc, values: valuesSrc}
}

// mergedCtx composes two parents: cancel signals from `cancel`, values from
// `values` then `cancel`. No goroutines or mutex — one allocation per message.
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
