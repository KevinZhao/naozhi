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

// withTraceID is an HTTP middleware (R247-ARCH-20, #677) that ensures
// every request carries a trace id in its context and the response
// echoes the same id back to the client.
//
// Behaviour:
//
//   - If the inbound request already has X-Request-ID set, we respect
//     it (use case: load-balancer / sidecar already stamped one). An
//     incoming id is treated as opaque — we do *not* validate the
//     length / charset because we never reflect it into a query, only
//     into a structured-log field and a response header that mirrors
//     it byte-for-byte.
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
		if id == "" {
			id = ctxutil.NewTraceID()
		}
		if id != "" {
			w.Header().Set(traceIDHeader, id)
			r = r.WithContext(ctxutil.WithTraceID(r.Context(), id))
		}
		next.ServeHTTP(w, r)
	})
}
