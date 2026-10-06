package ctxutil

import "context"

// Turn correlation (#3436): one IM message becomes a trace (WithTraceID at
// ingress), each turn the orchestrator runs gets a run id, and the session
// key names the CLI it ran on. All three ride the ctx so any package on the
// path can log them, and Handler adds them to every slog record logged with
// a ctx, so journalctl can be grepped by run_id without each call site
// repeating the fields.

type runIDKey struct{}
type sessionKeyKey struct{}

// WithRunID derives a context carrying the turn's run id; "" is a no-op.
func WithRunID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, runIDKey{}, id)
}

// RunID returns the run id in ctx, or "".
func RunID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(runIDKey{}).(string)
	return v
}

// WithSessionKey derives a context carrying the session key; "" is a no-op.
func WithSessionKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionKeyKey{}, key)
}

// SessionKey returns the session key in ctx, or "".
func SessionKey(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(sessionKeyKey{}).(string)
	return v
}
