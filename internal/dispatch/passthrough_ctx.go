package dispatch

import "context"

// passthroughCtxKey is the sentinel ctx value used by dispatch to opt a
// single turn into passthrough mode. The downstream sendFn (server's
// sendWithBroadcast) inspects this to decide between SendPassthrough and
// the legacy sendMu-serialized Send path.
//
// We place this key in the dispatch package — not server — so that dispatch
// can tag outgoing ctx without importing server (which would create a cycle).
type passthroughCtxKey struct{}

// WithPassthrough returns a ctx that signals the dispatch pipeline should use
// SendPassthrough downstream when the session's protocol supports replay.
// Without this marker every Send goes through the legacy serialized path.
func WithPassthrough(ctx context.Context) context.Context {
	return context.WithValue(ctx, passthroughCtxKey{}, true)
}

// IsPassthrough reports whether WithPassthrough was applied to this ctx.
func IsPassthrough(ctx context.Context) bool {
	v, _ := ctx.Value(passthroughCtxKey{}).(bool)
	return v
}

// urgentCtxKey signals the downstream send path to forward priority:"now"
// to the CLI. Must be combined with WithPassthrough — urgent without
// passthrough downgrades to legacy interrupt+send (the downstream sendFn
// handles the fallback).
type urgentCtxKey struct{}

// WithUrgent marks a ctx so sendWithBroadcast forwards priority:"now"
// to the CLI. The CLI aborts any in-flight turn and processes this message
// next. Pending slots that were enqueued before this urgent get
// ErrAbortedByUrgent; dispatch surfaces that to the user.
func WithUrgent(ctx context.Context) context.Context {
	return context.WithValue(ctx, urgentCtxKey{}, true)
}

// IsUrgent reports whether WithUrgent was applied.
func IsUrgent(ctx context.Context) bool {
	v, _ := ctx.Value(urgentCtxKey{}).(bool)
	return v
}
