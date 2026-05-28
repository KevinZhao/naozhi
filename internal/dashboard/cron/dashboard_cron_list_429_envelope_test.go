package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

)

// TestHandleList_429ResponseShape pins R242-CR-3 (#691): when GET /api/cron
// hits the per-IP listLimiter, the response MUST be a JSON envelope
// (Content-Type: application/json + `{"error":"…"}`) — NOT the default
// http.Error text/plain. The dashboard frontend distinguishes 429 from
// other 4xx by parsing the JSON body; a future refactor that swaps in
// http.Error(w, …, 429) would silently break the toast surface.
//
// Complements TestHandleList_PerIPRateLimit (which pins the gate itself)
// by locking down the response shape so a "limiter still works but body
// changed" regression cannot slip through.
func TestHandleList_429ResponseShape(t *testing.T) {
	t.Parallel()

	// Burst=1 so the second call is guaranteed to 429.
	h := &Handlers{
		listLimiter: newPerIPBurstNLimiter(1),
	}

	doReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/cron", nil)
		req.RemoteAddr = "10.1.2.3:5555"
		w := httptest.NewRecorder()
		h.HandleList(w, req)
		return w
	}
	_ = doReq() // burn the burst
	w := doReq()

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429; body=%q", w.Code, w.Body.String())
	}
	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Fatalf("Content-Type=%q, want application/json — #691 frontend parses JSON envelope", ct)
	}
	// Body MUST decode as JSON object with non-empty "error" string —
	// the dashboard's 429 toast reads this field.
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body=%q: not JSON: %v", w.Body.String(), err)
	}
	errMsg, _ := body["error"].(string)
	if errMsg == "" {
		t.Fatalf("body=%q: missing or empty error field", w.Body.String())
	}
}
