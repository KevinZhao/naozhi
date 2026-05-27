// Package ctxutil holds context.Context helpers shared across the
// HTTP / cli / dispatch boundaries.
//
// # trace_id propagation (R247-ARCH-20, #677)
//
// Postmortems on a multi-component request path (HTTP webhook → session
// router → cli child process → reply) currently grep timestamps to
// stitch a single user message together. There is no shared correlation
// id flowing across package boundaries.
//
// This package introduces a minimum-viable `WithTraceID(ctx, id)` /
// `TraceID(ctx)` pair plus a tiny `slog.Logger` helper. Once an HTTP
// middleware (or any other ingress) stamps a trace id on the request
// context, every downstream package can opportunistically `slog.With`
// the value into its derived logger without taking a hard dependency on
// the upstream package layout.
//
// The generation function is deliberately untrusted-input-safe: ids are
// 16 hex chars (64 bits of randomness) which is cheap to generate, fits
// in a single log field without wrapping, and is large enough to make
// collisions in any realistic 24-hour log window negligible.
package ctxutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
)

// traceIDKey is the context-value key for the request trace id. We use
// an unexported pointer-typed sentinel rather than a string so foreign
// packages cannot collide with our key by accident — the standard idiom
// for context-value keys.
type traceIDKey struct{}

// WithTraceID derives a context that carries the given trace id. An empty
// id is a no-op (returns the parent unchanged) so middleware that fails
// to generate an id doesn't poison downstream lookups with an empty
// string sentinel.
func WithTraceID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, traceIDKey{}, id)
}

// TraceID returns the trace id stored in ctx, or "" if no trace id has
// been attached. Callers should treat "" as "untraced" and skip the
// associated log field rather than emitting a literal empty string.
func TraceID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(traceIDKey{}).(string)
	return v
}

// LoggerWithTrace returns a derived slog.Logger that includes the
// `trace_id` attribute when one is present in ctx. If no trace id is
// attached the original logger is returned unchanged (no orphan
// `trace_id=""` field in the output).
//
// Use this at every package boundary the request crosses:
//
//	log := ctxutil.LoggerWithTrace(ctx, baseLogger)
//	log.Info("dispatching", "channel", channel)
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

// NewTraceID returns a freshly-minted 16-char hex trace id.
//
// We use 8 bytes of crypto/rand entropy because:
//   - 64 bits is overwhelmingly enough for de-duplication within any
//     realistic operations window;
//   - hex(8) renders as 16 chars which fits cleanly in a single log
//     column without wrapping in journalctl / `tail -f` views;
//   - crypto/rand is the only stdlib RNG that is safe to call from
//     concurrent middleware without seeding boilerplate.
//
// On the (effectively-impossible) crypto/rand failure path we return
// the empty string; the caller is expected to fall back to a
// caller-supplied id (e.g. an incoming `X-Request-ID` header).
func NewTraceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}
