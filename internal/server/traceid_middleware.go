package server

import (
	"net/http"

	"github.com/naozhi/naozhi/internal/ctxutil"
)

// traceIDHeader is the HTTP header consumed/emitted for cross-component
// request correlation. We accept the de-facto-standard `X-Request-ID`
// rather than inventing a new name so reverse-proxies (nginx, ALB) and
// API consumers can stamp the id upstream of naozhi and we'll honour it
// instead of overwriting with a fresh value.
const traceIDHeader = "X-Request-ID"

// maxInboundTraceIDLen caps how long an upstream-supplied X-Request-ID may
// be before we discard it and mint our own. An inbound id is opaque and
// flows into a structured-log field plus the echoed response header; an
// attacker (or a misbehaving proxy) could otherwise stamp a multi-kilobyte
// value that bloats every correlated log line and the response header. ALB,
// nginx, and the common UUID/hex conventions all stay well under 128 chars,
// so the cap never rejects a legitimately stamped id.
const maxInboundTraceIDLen = 128

// acceptInboundTraceID reports whether an upstream X-Request-ID is safe to
// propagate verbatim. We honour the de-facto correlation header but only
// when it is non-empty, within the length cap, and free of control bytes
// (so it cannot smuggle CR/LF into log records or downstream sinks even if
// a future writer is less strict than net/http's header sanitiser). On
// rejection the caller mints a fresh id instead.
func acceptInboundTraceID(id string) bool {
	if id == "" || len(id) > maxInboundTraceIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x20 || id[i] == 0x7f {
			return false
		}
	}
	return true
}

// withTraceID is an HTTP middleware (R247-ARCH-20, #677) that ensures
// every request carries a trace id in its context and the response
// echoes the same id back to the client.
//
// Behaviour:
//
//   - If the inbound request already has X-Request-ID set, we respect
//     it (use case: load-balancer / sidecar already stamped one) as long
//     as it passes acceptInboundTraceID — non-empty, within
//     maxInboundTraceIDLen, and free of control bytes. The id flows into
//     a structured-log field and an echoed response header, so an
//     oversized or control-byte-laden value would otherwise bloat every
//     correlated log line or risk header/log smuggling. A rejected id is
//     replaced with a freshly minted one.
//   - Otherwise we mint a 16-hex-char id via ctxutil.NewTraceID.
//   - The id is stamped into the request ctx so any downstream handler
//     (or anything reachable through ctx) can call ctxutil.TraceID(ctx)
//     and ctxutil.LoggerWithTrace(ctx, log) to enrich its logs.
//   - The id is also written to the response Header before the handler
//     runs, so even an early-returning handler (auth deny, 429, panic)
//     emits the correlation id back to the caller.
//
// Wired into the http.Server in server.go (Run) as the outermost
// handler so every request — authed and unauthed alike — sees a
// trace id on its ctx before any downstream middleware or handler
// runs. Per-package logger enrichment (slog.Logger.With(...)) lives
// in the consuming package; this middleware just guarantees an id
// exists on the ctx by the time the handler chain starts running.
// The middleware lives in its own file (rather than alongside
// withMaxBytes in middleware.go) so concurrent edits to the
// body-cap middleware don't churn this commit. R247-ARCH-20 / #677.
func withTraceID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(traceIDHeader)
		if !acceptInboundTraceID(id) {
			id = ctxutil.NewTraceID()
		}
		if id != "" {
			w.Header().Set(traceIDHeader, id)
			r = r.WithContext(ctxutil.WithTraceID(r.Context(), id))
		}
		next.ServeHTTP(w, r)
	})
}
