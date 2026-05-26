package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTranscriptScanInflight_AcquireReleaseRoundTrip pins R243-SEC-12 (#798)
// at the semaphore primitive: the package-level slot pool is the single
// shared budget and the acquire/release contract has to be reciprocal.
//
// Specifically:
//
//  1. Up to transcriptScanInflightCap acquires succeed in a row.
//  2. The (cap+1)th acquire fails fast (no blocking, no queue).
//  3. After releasing one slot, the next acquire succeeds again — the
//     pool is genuinely reusable, not a one-shot counter.
//
// We can't easily integration-test the handler-level "503 + Retry-After"
// path without a fully-wired CronHandlers (which needs a Scheduler, IP
// limiter, claudeDir, …). Pinning the primitive instead keeps the
// regression gate cheap while still locking the contract that the
// handler depends on.
func TestTranscriptScanInflight_AcquireReleaseRoundTrip(t *testing.T) {
	// Drain any residual slots from earlier tests so this run starts
	// with a known state. The pool is a package-level channel so other
	// tests in the same binary share it; we restore on cleanup.
	drained := 0
	for {
		select {
		case <-transcriptScanInflight:
			drained++
			continue
		default:
		}
		break
	}
	t.Cleanup(func() {
		// Restore exactly the number of slots we drained so subsequent
		// tests see the original buffered state.
		for i := 0; i < drained; i++ {
			transcriptScanInflight <- struct{}{}
		}
	})

	// Step 1: acquire all transcriptScanInflightCap slots back-to-back.
	for i := 0; i < transcriptScanInflightCap; i++ {
		if !acquireTranscriptScanSlot() {
			t.Fatalf("acquire %d/%d failed; cap=%d", i+1, transcriptScanInflightCap, transcriptScanInflightCap)
		}
	}

	// Step 2: the next acquire MUST fail immediately. If this blocks
	// instead of returning false, the test will time out — that's the
	// regression mode (someone replaced the non-blocking select with a
	// blocking send).
	if acquireTranscriptScanSlot() {
		t.Fatal("acquire beyond cap unexpectedly succeeded; concurrency budget is leaky")
	}

	// Step 3: release one slot and verify another caller can take it.
	releaseTranscriptScanSlot()
	if !acquireTranscriptScanSlot() {
		t.Fatal("acquire after release failed; release path likely returned the wrong direction")
	}

	// Cleanup: drain everything we hold so we don't leak slots into the
	// next test in this package.
	for i := 0; i < transcriptScanInflightCap; i++ {
		releaseTranscriptScanSlot()
	}
}

// TestTranscriptScanInflight_HandlerReturns503OnSaturation pins the
// handler-level contract: when the package-level slot pool is fully
// occupied, /api/cron/runs/.../transcript MUST short-circuit with
// 503 + Retry-After: 1, NOT 429 (per-IP rate limit) and NOT 200.
// Distinguishing 503/429 lets operators tell concurrency saturation
// from per-IP rate-limit hits in logs without having to grep the body.
//
// We saturate by pre-filling the channel directly — no need to spin up
// real Scheduler/handlers. The handler's first acquire fires before any
// other state is consulted, so a zero-state CronHandlers reaches that
// branch.
func TestTranscriptScanInflight_HandlerReturns503OnSaturation(t *testing.T) {
	// Snapshot + drain the pool, then fill it to cap. Restore on exit so
	// other tests are unaffected.
	drained := 0
	for {
		select {
		case <-transcriptScanInflight:
			drained++
			continue
		default:
		}
		break
	}
	for i := 0; i < transcriptScanInflightCap; i++ {
		transcriptScanInflight <- struct{}{}
	}
	t.Cleanup(func() {
		// Empty the pool, then restore the original level.
		for {
			select {
			case <-transcriptScanInflight:
				continue
			default:
			}
			break
		}
		for i := 0; i < drained; i++ {
			transcriptScanInflight <- struct{}{}
		}
	})

	h := &CronHandlers{}
	r := httptest.NewRequest(http.MethodGet, "/api/cron/runs/abc/transcript?job_id=def", nil)
	// PathValue must be set explicitly when the request bypasses ServeMux.
	r.SetPathValue("run_id", "abc")
	w := httptest.NewRecorder()

	h.handleRunTranscript(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want %q", got, "1")
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}
