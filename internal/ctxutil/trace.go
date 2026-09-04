// Package ctxutil holds context.Context helpers shared across the
// HTTP / cli / dispatch boundaries.
//
// WithTraceID / TraceID carry a per-request trace id so every downstream
// package can slog.With it without depending on the ingress layout (#677).
package ctxutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
)

// traceIDKey is an unexported sentinel type so foreign packages cannot collide
// with the context key.
type traceIDKey struct{}

// WithTraceID derives a context carrying id. An empty id returns ctx unchanged
// so downstream lookups never see an empty-string sentinel.
func WithTraceID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, traceIDKey{}, id)
}

// TraceID returns the trace id stored in ctx, or "" when untraced; callers
// should skip the log field rather than emit an empty string.
func TraceID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(traceIDKey{}).(string)
	return v
}

// LoggerWithTrace returns logger with a `trace_id` attribute when ctx carries
// one, otherwise logger unchanged.
func LoggerWithTrace(ctx context.Context, logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return nil
	}
	id := TraceID(ctx)
	if id == "" {
		return logger
	}
	return logger.With("trace_id", id)
}

// NewTraceID returns a fresh 16-char hex id (8 bytes of crypto/rand: enough to
// avoid collisions in any realistic log window, fits one log column). Returns
// "" on the crypto/rand failure path; callers fall back to a supplied id.
func NewTraceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}
