package cron

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleCreate_PerIPRateLimit pins R053116-SEC-3: POST /api/cron MUST
// gate per-IP via writeLimiter before reaching the scheduler. A stolen
// dashboard token without this gate could hammer cron_jobs.json IO and the
// scheduler map via repeated creates. Burst=2 + rate.Every(time.Hour) means
// the third call inside the test window MUST hit 429.
//
// Scheduler is left nil so requests that pass the limiter fall through to
// the 501 path — the load-bearing assertion is 429 not 501 on the third
// request: an inverted gate would 501 before 429.
func TestHandleCreate_PerIPRateLimit(t *testing.T) {
	t.Parallel()
	h := &Handlers{
		writeLimiter: newPerIPBurstNLimiter(2),
	}
	body := `{"schedule":"@hourly","prompt":"hi"}`
	doReq := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/cron", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "10.0.0.1:1234"
		w := httptest.NewRecorder()
		h.HandleCreate(w, req)
		return w.Code
	}
	for i := 0; i < 2; i++ {
		if got := doReq(); got == http.StatusTooManyRequests {
			t.Fatalf("request %d 429ed early — burst budget exhausted prematurely", i+1)
		}
	}
	if got := doReq(); got != http.StatusTooManyRequests {
		t.Fatalf("3rd create after burst exhaustion: got %d, want 429; R053116-SEC-3 create rate-limit gate may be missing", got)
	}
}

// TestHandleCreate_NilLimiter_PassThrough mirrors HandleTrigger's nil-guard
// test: when no writeLimiter is wired, HandleCreate must NOT 429 — it should
// fall through to the scheduler-nil 501 path.
func TestHandleCreate_NilLimiter_PassThrough(t *testing.T) {
	t.Parallel()
	h := &Handlers{} // writeLimiter nil
	req := httptest.NewRequest(http.MethodPost, "/api/cron", strings.NewReader(`{"schedule":"@hourly","prompt":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)
	if w.Code == http.StatusTooManyRequests {
		t.Fatalf("nil writeLimiter must not produce 429; got %d", w.Code)
	}
}

// TestHandleDelete_PerIPRateLimit pins R053116-SEC-3 for DELETE /api/cron.
func TestHandleDelete_PerIPRateLimit(t *testing.T) {
	t.Parallel()
	h := &Handlers{
		writeLimiter: newPerIPBurstNLimiter(2),
	}
	doReq := func() int {
		req := httptest.NewRequest(http.MethodDelete, "/api/cron?id=deadbeefdeadbeef", nil)
		req.RemoteAddr = "10.0.0.2:5678"
		w := httptest.NewRecorder()
		h.HandleDelete(w, req)
		return w.Code
	}
	for i := 0; i < 2; i++ {
		if got := doReq(); got == http.StatusTooManyRequests {
			t.Fatalf("request %d 429ed early", i+1)
		}
	}
	if got := doReq(); got != http.StatusTooManyRequests {
		t.Fatalf("3rd delete after burst exhaustion: got %d, want 429; R053116-SEC-3 delete rate-limit gate may be missing", got)
	}
}

// TestHandleDelete_NilLimiter_PassThrough: nil writeLimiter must not 429.
func TestHandleDelete_NilLimiter_PassThrough(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	req := httptest.NewRequest(http.MethodDelete, "/api/cron?id=deadbeefdeadbeef", nil)
	w := httptest.NewRecorder()
	h.HandleDelete(w, req)
	if w.Code == http.StatusTooManyRequests {
		t.Fatalf("nil writeLimiter must not produce 429; got %d", w.Code)
	}
}

// TestHandlePause_PerIPRateLimit pins R053116-SEC-3 for POST /api/cron/pause.
func TestHandlePause_PerIPRateLimit(t *testing.T) {
	t.Parallel()
	h := &Handlers{
		writeLimiter: newPerIPBurstNLimiter(2),
	}
	body := `{"id":"deadbeefdeadbeef"}`
	doReq := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/cron/pause", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "10.0.0.3:9090"
		w := httptest.NewRecorder()
		h.HandlePause(w, req)
		return w.Code
	}
	for i := 0; i < 2; i++ {
		if got := doReq(); got == http.StatusTooManyRequests {
			t.Fatalf("request %d 429ed early", i+1)
		}
	}
	if got := doReq(); got != http.StatusTooManyRequests {
		t.Fatalf("3rd pause after burst exhaustion: got %d, want 429; R053116-SEC-3 pause rate-limit gate may be missing", got)
	}
}

// TestHandlePause_NilLimiter_PassThrough: nil writeLimiter must not 429.
func TestHandlePause_NilLimiter_PassThrough(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	req := httptest.NewRequest(http.MethodPost, "/api/cron/pause", strings.NewReader(`{"id":"deadbeefdeadbeef"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandlePause(w, req)
	if w.Code == http.StatusTooManyRequests {
		t.Fatalf("nil writeLimiter must not produce 429; got %d", w.Code)
	}
}

// TestHandleResume_PerIPRateLimit pins R053116-SEC-3 for POST /api/cron/resume.
func TestHandleResume_PerIPRateLimit(t *testing.T) {
	t.Parallel()
	h := &Handlers{
		writeLimiter: newPerIPBurstNLimiter(2),
	}
	body := `{"id":"deadbeefdeadbeef"}`
	doReq := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/cron/resume", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "10.0.0.4:7777"
		w := httptest.NewRecorder()
		h.HandleResume(w, req)
		return w.Code
	}
	for i := 0; i < 2; i++ {
		if got := doReq(); got == http.StatusTooManyRequests {
			t.Fatalf("request %d 429ed early", i+1)
		}
	}
	if got := doReq(); got != http.StatusTooManyRequests {
		t.Fatalf("3rd resume after burst exhaustion: got %d, want 429; R053116-SEC-3 resume rate-limit gate may be missing", got)
	}
}

// TestHandleResume_NilLimiter_PassThrough: nil writeLimiter must not 429.
func TestHandleResume_NilLimiter_PassThrough(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	req := httptest.NewRequest(http.MethodPost, "/api/cron/resume", strings.NewReader(`{"id":"deadbeefdeadbeef"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleResume(w, req)
	if w.Code == http.StatusTooManyRequests {
		t.Fatalf("nil writeLimiter must not produce 429; got %d", w.Code)
	}
}
