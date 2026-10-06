package ctxutil

import (
	"context"
	"log/slog"
)

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

// Handler wraps a slog.Handler and appends trace_id / run_id / session_key
// from the record's ctx when present. Records logged without a ctx (the
// package-level slog.Info family) pass through unchanged.
type Handler struct{ inner slog.Handler }

// NewHandler wraps inner.
func NewHandler(inner slog.Handler) *Handler { return &Handler{inner: inner} }

// Enabled defers to the wrapped handler.
func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool { return h.inner.Enabled(ctx, l) }

// Handle adds the correlation attrs the ctx carries, then delegates.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if id := TraceID(ctx); id != "" {
		r.AddAttrs(slog.String("trace_id", id))
	}
	if id := RunID(ctx); id != "" {
		r.AddAttrs(slog.String("run_id", id))
	}
	if key := SessionKey(ctx); key != "" {
		r.AddAttrs(slog.String("session_key", key))
	}
	return h.inner.Handle(ctx, r)
}

// Unwrap returns the wrapped handler (tests inspect the configured format).
func (h *Handler) Unwrap() slog.Handler { return h.inner }

// WithAttrs wraps the inner handler's WithAttrs.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{inner: h.inner.WithAttrs(attrs)}
}

// WithGroup wraps the inner handler's WithGroup.
func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{inner: h.inner.WithGroup(name)}
}
