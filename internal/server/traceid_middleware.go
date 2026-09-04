package server

import (
	"net/http"

	"github.com/naozhi/naozhi/internal/ctxutil"
)

// traceIDHeader is the de-facto-standard correlation header, so reverse
// proxies can stamp an id upstream and naozhi honours it.
const traceIDHeader = "X-Request-ID"

// withTraceID ensures every request carries a trace id in its context and
// the response echoes the same id (#677).
//
// An inbound X-Request-ID is respected and treated as opaque (never
// validated: it only reaches a structured-log field and the mirrored
// response header). Otherwise ctxutil.NewTraceID mints one. The header is
// set before the handler runs so early returns (auth deny, 429) carry it.
// Wired as the outermost handler in server.go.
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
