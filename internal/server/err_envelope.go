package server

import (
	"net/http"
	"strconv"
)

// errEnvelope is the unified JSON shape for dashboard error responses.
// First-wave migration helper for R247-ARCH-3 (#612): the dashboard
// previously mixed http.Error (text/plain) at ~216 sites with ad-hoc
// `map[string]string{"error": ...}` JSON envelopes (~110 sites),
// forcing the front-end to branch on Content-Type for every error
// path. Centralising the wire shape here means trace_id and
// retry_after can finally travel through error responses (plain-text
// http.Error never carried them).
//
// Field rationale:
//
//   - Error: legacy field name preserved verbatim so existing
//     dashboard.js code paths reading `body.error` keep working
//     across the transition window.
//   - Code: machine-readable token (e.g. "rate_limited", "not_found")
//     so the front-end can branch without parsing English copy.
//     omitempty during migration since not every legacy site has a
//     vocabulary-stable code yet.
//   - TraceID / RetryAfter: optional structured context plain-text
//     http.Error replies never carried. RetryAfter mirrors the
//     Retry-After header in seconds — front-end can drive a single
//     countdown from the body, regardless of whether a fetch wrapper
//     dropped headers.
type errEnvelope struct {
	Error      string `json:"error"`
	Code       string `json:"code,omitempty"`
	TraceID    string `json:"trace_id,omitempty"`
	RetryAfter int    `json:"retry_after,omitempty"`
}

// errResp writes a unified JSON error envelope. Direct replacement for
// `http.Error(w, msg, status)` at the call site. R247-ARCH-3 (#612).
//
// Pass a stable code from a closed vocabulary (empty allowed during
// migration). Keep msg short — operator-facing copy, not telemetry.
//
// Migration scope: this is the first wave (a small handful of
// dashboard.go sites). Remaining sites stay on http.Error during the
// transition; once all sites have moved, http.Error usage in this
// package can be linted out via a source-level contract test.
//
// Implementation borrows writeJSONStatus (defined in dashboard.go) so
// the Content-Type / X-Content-Type-Options / Cache-Control header
// trio stays consistent with every other JSON reply, and the pooled
// encoder is shared. Keeping the helper in a separate file (not
// dashboard.go) reduces the merge-conflict surface during the wave-
// by-wave migration of the 216 legacy sites.
func errResp(w http.ResponseWriter, status int, code, msg string) {
	writeJSONStatus(w, status, errEnvelope{Error: msg, Code: code})
}

// errRespRetry is the rate-limit / 503 variant: same envelope plus
// the Retry-After header AND the structured retry_after field, so
// callers that don't read response headers (most JS fetch wrappers
// ignore them) still get the back-off hint. R247-ARCH-3 (#612).
//
// retryAfterSeconds <= 0 elides both the header and the body field
// so the helper is safe to call from sites that don't actually carry
// a server-suggested back-off (e.g. a degraded-mode 503 with no
// scheduled recovery window).
func errRespRetry(w http.ResponseWriter, status int, code, msg string, retryAfterSeconds int) {
	if retryAfterSeconds > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	}
	writeJSONStatus(w, status, errEnvelope{
		Error:      msg,
		Code:       code,
		RetryAfter: retryAfterSeconds,
	})
}
