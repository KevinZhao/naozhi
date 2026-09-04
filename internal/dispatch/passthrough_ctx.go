package dispatch

import "context"

// sendOpts carries the passthrough+urgent decisions as a single ctx value
// under sendOptsCtxKey so readers do one ctx.Value lookup (#786).
type sendOpts struct {
	Passthrough bool
	Urgent      bool
}

// sendOptsCtxKey lives in dispatch (not server) so dispatch can tag
// outgoing ctx without importing server (cycle).
type sendOptsCtxKey struct{}

// withSendOpts attaches opts to ctx.
func withSendOpts(ctx context.Context, opts sendOpts) context.Context {
	return context.WithValue(ctx, sendOptsCtxKey{}, opts)
}

// sendOptsFromContext returns the attached sendOpts (zero value if none).
func sendOptsFromContext(ctx context.Context) sendOpts {
	o, _ := ctx.Value(sendOptsCtxKey{}).(sendOpts)
	return o
}

// WithPassthrough returns a ctx that signals the dispatch pipeline should use
// SendPassthrough downstream when the session's protocol supports replay.
// Without this marker every Send goes through the legacy serialized path.
func WithPassthrough(ctx context.Context) context.Context {
	o := sendOptsFromContext(ctx)
	o.Passthrough = true
	return withSendOpts(ctx, o)
}

// IsPassthrough reports whether WithPassthrough was applied to this ctx.
func IsPassthrough(ctx context.Context) bool {
	return sendOptsFromContext(ctx).Passthrough
}

// WithUrgent marks a ctx so sendWithBroadcast forwards priority:"now"
// to the CLI. The CLI aborts any in-flight turn and processes this message
// next. Pending slots that were enqueued before this urgent get
// ErrAbortedByUrgent; dispatch surfaces that to the user.
//
// Must be combined with WithPassthrough — urgent without passthrough
// downgrades to legacy interrupt+send (the downstream sendFn handles the
// fallback).
func WithUrgent(ctx context.Context) context.Context {
	o := sendOptsFromContext(ctx)
	o.Urgent = true
	return withSendOpts(ctx, o)
}

// IsUrgent reports whether WithUrgent was applied.
func IsUrgent(ctx context.Context) bool {
	return sendOptsFromContext(ctx).Urgent
}
