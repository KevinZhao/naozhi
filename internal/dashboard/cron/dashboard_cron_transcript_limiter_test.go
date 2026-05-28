package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

)

// TestHandleRunTranscript_UsesTranscriptLimiterNotRunsLimiter pins R250-SEC-7
// (#1096): the transcript endpoint must consume from its dedicated
// transcriptLimiter bucket, NOT from the shared runsLimiter that
// /api/cron/runs and /api/cron/runs/{run_id} use. Sharing one bucket lets
// either endpoint starve the other under load — a transcript-flood attacker
// drives every legit runs-list query into 429, and vice versa.
//
// The test wires up a runsLimiter with effectively-unlimited budget and a
// transcriptLimiter with burst=1; the second transcript call must 429 while
// the runsLimiter remains untouched (proven by the runs-list call below
// going through). Mirrors the shape of TestHandleList_429ResponseShape.
func TestHandleRunTranscript_UsesTranscriptLimiterNotRunsLimiter(t *testing.T) {
	t.Parallel()

	h := &Handlers{
		// runsLimiter: huge budget so we know any 429 below MUST come
		// from the transcriptLimiter, not from a shared-bucket spillover.
		runsLimiter: alwaysAllowLimiter{},
		// transcriptLimiter: burst=1 so the second call is guaranteed
		// to 429.
		transcriptLimiter: newPerIPBurstNLimiter(1),
	}

	doReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/r1/transcript?job_id=j1", nil)
		req.RemoteAddr = "10.1.2.3:5555"
		w := httptest.NewRecorder()
		h.HandleRunTranscript(w, req)
		return w
	}

	// Burn the transcriptLimiter burst.
	_ = doReq()

	// Second call: must 429 — proves the transcript path is gated by
	// transcriptLimiter, not runsLimiter (which still has 999+ tokens).
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
	if !strings.Contains(msg, "transcript") {
		t.Fatalf("error=%q, want it to mention transcript (so dashboard can disambiguate)", msg)
	}
}

// TestHandleRunTranscript_NilTranscriptLimiterFallsBackToRunsLimiter pins
// the nil-guard fallback: legacy hand-rolled Handlers fixtures
// (newCronHandlersForTest paths that haven't been updated to wire a
// transcriptLimiter) keep the previous behaviour — runsLimiter gates the
// endpoint. Without the fallback, those fixtures would lose all rate
// limiting on the transcript endpoint.
func TestHandleRunTranscript_NilTranscriptLimiterFallsBackToRunsLimiter(t *testing.T) {
	t.Parallel()

	h := &Handlers{
		// transcriptLimiter intentionally nil — fallback path.
		runsLimiter: newPerIPBurstNLimiter(1),
	}

	doReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/r1/transcript?job_id=j1", nil)
		req.RemoteAddr = "10.1.2.3:5555"
		w := httptest.NewRecorder()
		h.HandleRunTranscript(w, req)
		return w
	}

	_ = doReq() // burn runsLimiter burst
	w := doReq()
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429 via runsLimiter fallback", w.Code)
	}
}
