package server

import (
	"context"
	"net/http/httptest"
	"testing"
)

// TestHandleRunTranscript_SemaphoreFastFails503 pins the contract that
// when the transcript concurrency semaphore is full, an additional
// request returns 503 immediately without waiting for the request
// context to expire. R243-SEC-12 (#798): without the gate, N
// authenticated operators can each saturate their per-IP runsLimiter
// and pile up N×(8 MB LimitReader + 256 KB scanner buffer) of
// resident memory. The semaphore puts a process-wide ceiling on that
// concurrency.
//
// The test fills the semaphore by hand (no real cron run needed) and
// drives a HEAD-shaped request through handleRunTranscript; the
// response status pins the 503 path.
func TestHandleRunTranscript_SemaphoreFastFails503(t *testing.T) {
	// Cap of 1 keeps the test fast — fill the slot ourselves so the
	// next call has to take the default branch.
	h := &CronHandlers{
		transcriptSem: make(chan struct{}, 1),
	}
	h.transcriptSem <- struct{}{} // slot full

	r := httptest.NewRequest("GET", "/api/cron/runs/r1/transcript?job_id=j1", nil)
	w := httptest.NewRecorder()
	h.handleRunTranscript(w, r)

	if got, want := w.Code, 503; got != want {
		t.Fatalf("status = %d, want %d (transcript-busy fast-fail)", got, want)
	}
	if body := w.Body.String(); !containsSubstrTranscriptSem(body, "transcript busy") {
		t.Fatalf("body = %q, want it to mention transcript busy", body)
	}
}

// TestHandleRunTranscript_NilSemaphoreSkipsGate pins that fixtures
// without a wired transcriptSem (newCronHandlersForTest) skip the
// concurrency gate and reach the existing handler logic. The handler
// will still 503 here because scheduler is nil, but specifically it
// must NOT hit the "transcript busy" path — that confirms the
// nil-guard short-circuits before sem acquisition.
func TestHandleRunTranscript_NilSemaphoreSkipsGate(t *testing.T) {
	h := &CronHandlers{} // transcriptSem nil → gate disabled

	r := httptest.NewRequest("GET", "/api/cron/runs/r1/transcript?job_id=j1", nil)
	w := httptest.NewRecorder()
	h.handleRunTranscript(w, r)

	// scheduler is nil → 501 "cron not configured", NOT 503 "busy".
	// The exact 501 path proves the sem gate did not fire.
	if got := w.Code; got == 503 {
		t.Fatalf("status = 503; nil sem must NOT take the busy fast-fail path")
	}
	if body := w.Body.String(); containsSubstrTranscriptSem(body, "transcript busy") {
		t.Fatalf("body unexpectedly says transcript busy with nil sem: %q", body)
	}
}

// TestHandleRunTranscript_SemaphoreReleasedOnReturn pins the defer
// release: after the handler returns, the slot must be available
// again. Without the defer, every request would leak a slot and the
// gate would lock up after `cap` requests.
func TestHandleRunTranscript_SemaphoreReleasedOnReturn(t *testing.T) {
	h := &CronHandlers{
		transcriptSem: make(chan struct{}, 1),
	}

	// First call drains the slot (handler returns ~immediately due to
	// nil scheduler). On return the defer must release the slot.
	r := httptest.NewRequest("GET", "/api/cron/runs/r1/transcript?job_id=j1", nil)
	w := httptest.NewRecorder()
	h.handleRunTranscript(w, r)

	// Second call must be able to acquire — confirms the slot was
	// released. We probe via a non-blocking send on the channel.
	select {
	case h.transcriptSem <- struct{}{}:
		// expected — slot was released after first handler returned
		<-h.transcriptSem
	default:
		t.Fatal("slot not released after handler return; defer leaked")
	}
}

// TestHandleRunTranscript_RequestCancelDuringFullSem pins that when
// the sem is full AND the request context is already cancelled, the
// handler still 503s rather than blocking. The select has both a
// `default:` and a `<-ctx.Done():` arm so this path is reachable on
// pre-cancelled requests.
func TestHandleRunTranscript_RequestCancelDuringFullSem(t *testing.T) {
	h := &CronHandlers{
		transcriptSem: make(chan struct{}, 1),
	}
	h.transcriptSem <- struct{}{} // slot full

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel
	r := httptest.NewRequest("GET", "/api/cron/runs/r1/transcript?job_id=j1", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	h.handleRunTranscript(w, r)

	if got, want := w.Code, 503; got != want {
		t.Fatalf("status = %d, want %d (cancelled+full → busy)", got, want)
	}
}

func containsSubstrTranscriptSem(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
