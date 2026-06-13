package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleRunEvents_UsesTranscriptLimiterNotRunsLimiter pins R20260613-SEC-1:
// the events endpoint must consume from its dedicated transcriptLimiter bucket,
// NOT from the shared runsLimiter. The events path uses bufio.Scanner (16.5 MB
// max token) which is I/O-heavy like the transcript endpoint — sharing one
// bucket lets either endpoint starve the other under load.
//
// The test wires a runsLimiter with unlimited budget and a transcriptLimiter
// with burst=1; the second events call must 429 while the runsLimiter remains
// untouched. Mirrors TestHandleRunTranscript_UsesTranscriptLimiterNotRunsLimiter.
func TestHandleRunEvents_UsesTranscriptLimiterNotRunsLimiter(t *testing.T) {
	t.Parallel()

	h := &Handlers{
		// runsLimiter: huge budget so any 429 below must come from
		// transcriptLimiter, not from a shared-bucket spillover.
		runsLimiter: alwaysAllowLimiter{},
		// transcriptLimiter: burst=1 so the second call is guaranteed 429.
		transcriptLimiter: newPerIPBurstNLimiter(1),
	}

	doReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet,
			"/api/cron/runs/"+strings.Repeat("a", 16)+"/events?job_id="+strings.Repeat("b", 16), nil)
		req.RemoteAddr = "10.1.2.3:5555"
		w := httptest.NewRecorder()
		h.HandleRunEvents(w, req)
		return w
	}

	// Burn the transcriptLimiter burst.
	_ = doReq()

	// Second call: must 429 — proves events path is gated by
	// transcriptLimiter, not runsLimiter (which still has unlimited tokens).
	w := doReq()
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second call: got %d, want 429; body=%q", w.Code, w.Body.String())
	}
	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Fatalf("Content-Type=%q, want application/json", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body=%q: not JSON: %v", w.Body.String(), err)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "events") {
		t.Fatalf("error=%q, want it to mention events", msg)
	}
}

// TestHandleRunEvents_NilTranscriptLimiterFallsBackToRunsLimiter pins the
// nil-guard fallback: legacy Handlers fixtures without a transcriptLimiter
// keep their rate limiting via runsLimiter.
func TestHandleRunEvents_NilTranscriptLimiterFallsBackToRunsLimiter(t *testing.T) {
	t.Parallel()

	h := &Handlers{
		// transcriptLimiter intentionally nil — fallback path.
		runsLimiter: newPerIPBurstNLimiter(1),
	}

	doReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet,
			"/api/cron/runs/"+strings.Repeat("a", 16)+"/events?job_id="+strings.Repeat("b", 16), nil)
		req.RemoteAddr = "10.1.2.3:5555"
		w := httptest.NewRecorder()
		h.HandleRunEvents(w, req)
		return w
	}

	_ = doReq() // burn runsLimiter burst
	w := doReq()
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429 via runsLimiter fallback", w.Code)
	}
}
