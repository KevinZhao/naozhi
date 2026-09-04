package server

import (
	"net/http"
	"strconv"
)

// errEnvelope is the unified JSON shape for dashboard error responses (#612).
// Error keeps the legacy field name dashboard.js reads as `body.error`; Code
// is a machine-readable token (omitempty while legacy sites lack one);
// RetryAfter mirrors the Retry-After header in seconds so the body alone can
// drive a countdown even when a fetch wrapper drops headers.
type errEnvelope struct {
	Error      string `json:"error"`
	Code       string `json:"code,omitempty"`
	TraceID    string `json:"trace_id,omitempty"`
	RetryAfter int    `json:"retry_after,omitempty"`
}

// errResp writes a unified JSON error envelope. Drop-in for
// `http.Error(w, msg, status)` (#612). Pass a stable code from a closed
// vocabulary (empty allowed); keep msg short — operator-facing copy, not
// telemetry. Goes through writeJSONStatus so the Content-Type /
// X-Content-Type-Options / Cache-Control headers match every other JSON reply.
func errResp(w http.ResponseWriter, status int, code, msg string) {
	writeJSONStatus(w, status, errEnvelope{Error: msg, Code: code})
}

// errRespRetry is the rate-limit / 503 variant: same envelope plus the
// Retry-After header AND the retry_after body field (most JS fetch wrappers
// ignore headers). retryAfterSeconds <= 0 elides both, so it is safe for
// sites with no server-suggested back-off.
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
