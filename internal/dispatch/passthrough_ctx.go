package dispatch

import "context"

// sendOpts is the explicit struct R246-ARCH-10 (#786) called for: instead
// of two separate context.Value entries (one per boolean) we attach a
// single struct value under sendOptsCtxKey.  The public With/Is helpers
// below preserve backward compatibility — every existing caller keeps
// working — but downstream readers now do a single ctx.Value lookup +
// struct deref rather than two separate type-assertion dances.
//
// The struct is also the migration target: future call-site refactors
// inside dispatch can build a SendOpts directly and pass it through the
// helper that materialises a ctx, while cross-package consumers (server)
// can either stick with the legacy IsPassthrough/IsUrgent getters or
// migrate to SendOptsFromContext for one round-trip lookup.
type sendOpts struct {
	Passthrough bool
	Urgent      bool
}

// sendOptsCtxKey is the single ctx key dispatch uses to attach
// passthrough+urgent decisions.  Replaces the previous two-key shape
// (passthroughCtxKey + urgentCtxKey).  We place this key in the dispatch
// package — not server — so that dispatch can tag outgoing ctx without
// importing server (which would create a cycle).
type sendOptsCtxKey struct{}

// withSendOpts attaches opts to ctx.  Internal helper so callers within
// dispatch can construct the struct once and avoid the two-step
// WithPassthrough(WithUrgent(...)) chain.
func withSendOpts(ctx context.Context, opts sendOpts) context.Context {
	return context.WithValue(ctx, sendOptsCtxKey{}, opts)
}

// sendOptsFromContext returns the attached sendOpts (or the zero value
// if none was attached).  Single ctx.Value lookup; both booleans come
// out of one struct.
func sendOptsFromContext(ctx context.Context) sendOpts {
	o, _ := ctx.Value(sendOptsCtxKey{}).(sendOpts)
	return o
}

// WithPassthrough returns a ctx that signals the dispatch pipeline should use
// SendPassthrough downstream when the session's protocol supports replay.
// Without this marker every Send goes through the legacy serialized path.
//
// Internally this updates the merged sendOpts struct so a subsequent
// WithUrgent or any downstream IsUrgent / IsPassthrough lookup reads
// through a single ctx.Value entry — preserving the legacy public API
// while shrinking the per-Send type-assertion cost.
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
