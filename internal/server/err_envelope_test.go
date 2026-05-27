package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestErrResp_WireShape pins the unified envelope from #612 (R247-ARCH-3):
// errResp must write Content-Type: application/json with a body that
// decodes to {"error":..., "code":...}. Front-end relies on this shape
// — a regression that swaps in http.Error(text/plain) would silently
// break every error toast.
func TestErrResp_WireShape(t *testing.T) {
	w := httptest.NewRecorder()
	errResp(w, http.StatusBadRequest, "bad_input", "missing field")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if nosniff := w.Header().Get("X-Content-Type-Options"); nosniff != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff (defence-in-depth)", nosniff)
	}
	var body errEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body=%q: not JSON: %v", w.Body.String(), err)
	}
	if body.Error != "missing field" {
		t.Errorf("error = %q, want 'missing field'", body.Error)
	}
	if body.Code != "bad_input" {
		t.Errorf("code = %q, want 'bad_input'", body.Code)
	}
	if body.RetryAfter != 0 {
		t.Errorf("retry_after = %d, want 0 (omitempty for non-rate-limit)", body.RetryAfter)
	}
}

// TestErrRespRetry_SetsHeaderAndBody pins both surfaces for the
// rate-limit variant: clients that read headers see Retry-After,
// clients that only read JSON see retry_after. Single source of truth
// avoids the front-end branching mismatch documented in #612.
func TestErrRespRetry_SetsHeaderAndBody(t *testing.T) {
	w := httptest.NewRecorder()
	errRespRetry(w, http.StatusTooManyRequests, "rate_limited", "slow down", 30)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", w.Code)
	}
	if ra := w.Header().Get("Retry-After"); ra != "30" {
		t.Errorf("Retry-After header = %q, want '30'", ra)
	}
	var body errEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body=%q: not JSON: %v", w.Body.String(), err)
	}
	if body.RetryAfter != 30 {
		t.Errorf("retry_after body field = %d, want 30", body.RetryAfter)
	}
	if body.Code != "rate_limited" {
		t.Errorf("code = %q, want 'rate_limited'", body.Code)
	}
}

// TestErrRespRetry_ZeroSecondsElidesHeader pins the contract that a
// sentinel zero retry budget produces no Retry-After header and an
// omitempty body field — degraded-mode 503 with no recovery window
// shouldn't lie about a fixed back-off.
func TestErrRespRetry_ZeroSecondsElidesHeader(t *testing.T) {
	w := httptest.NewRecorder()
	errRespRetry(w, http.StatusServiceUnavailable, "degraded", "later", 0)

	if w.Header().Get("Retry-After") != "" {
		t.Errorf("Retry-After should be unset for retryAfterSeconds=0")
	}
	// Body must NOT carry retry_after when zero (omitempty).
	if strings.Contains(w.Body.String(), `"retry_after"`) {
		t.Errorf("body must omit retry_after when zero, got %q", w.Body.String())
	}
}

// TestErrEnvelope_ErrorFieldName pins the legacy field name so the
// dashboard.js central error handler keeps working across the
// transition. A future rename of `error` → `message` would silently
// break every toast — this asserts the JSON tag explicitly.
func TestErrEnvelope_ErrorFieldName(t *testing.T) {
	w := httptest.NewRecorder()
	errResp(w, http.StatusNotFound, "missing", "gone")

	body := w.Body.String()
	if !strings.Contains(body, `"error":"gone"`) {
		t.Errorf("body must carry top-level `error` key (legacy contract): got %q", body)
	}
}
