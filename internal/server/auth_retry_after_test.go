package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleLogin_Sets429AndRetryAfterOnRateLimit pins the server-side
// contract that the front-end R110-P2 WS auth rate-limit countdown
// relies on. saveToken in dashboard.js branches on `r.status === 429`
// and reads `r.headers.get('Retry-After')`; if either drifts the
// countdown logic silently degrades to the legacy "invalid token"
// misdiagnosis. The limiter is constructed with burst=5 so 6 rapid
// POSTs from the same IP are enough to drive the bucket empty and
// force the rate-limit branch, independently of wall-clock timing.
func TestHandleLogin_Sets429AndRetryAfterOnRateLimit(t *testing.T) {
	t.Parallel()
	a := &AuthHandlers{
		dashboardToken: "secret",
		cookieSecret:   []byte("cookie"),
		loginLimiter:   newLoginLimiter(),
	}

	const sameOriginIP = "10.0.0.42:54321"
	// Drain the burst with intentionally-wrong tokens so the successful
	// token path isn't cached or otherwise short-circuits. All 5 burst
	// requests must reach the token comparison (401) rather than 429,
	// proving the limiter's burst=5 is spent on token-compare work.
	for i := 0; i < 5; i++ {
		r := httptest.NewRequest(http.MethodPost, "http://naozhi.example/api/auth/login",
			strings.NewReader(`{"token":"wrong"}`))
		r.Host = "naozhi.example"
		r.RemoteAddr = sameOriginIP
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		a.handleLogin(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("burst req %d: status = %d, want 401 (body=%q)",
				i, w.Code, strings.TrimSpace(w.Body.String()))
		}
		if w.Header().Get("Retry-After") != "" {
			t.Errorf("burst req %d: Retry-After leaked on 401 path (= %q)", i, w.Header().Get("Retry-After"))
		}
	}

	// The 6th request must hit the rate limit.
	r := httptest.NewRequest(http.MethodPost, "http://naozhi.example/api/auth/login",
		strings.NewReader(`{"token":"secret"}`))
	r.Host = "naozhi.example"
	r.RemoteAddr = sameOriginIP
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.handleLogin(w, r)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("post-burst status = %d, want 429 (body=%q)",
			w.Code, strings.TrimSpace(w.Body.String()))
	}
	// Retry-After MUST be an integer of seconds per RFC 7231. Front-end
	// parses via parseInt so any non-integer would render NaN and fall
	// back to the 60s default — still acceptable but noisy in logs.
	ra := w.Header().Get("Retry-After")
	if ra == "" {
		t.Fatal("Retry-After header missing on 429 response; front-end countdown relies on it")
	}
	if ra != "60" {
		// Not strictly required, but any change here must be intentional
		// so the pinned value doubles as documentation.
		t.Errorf("Retry-After = %q, want %q (update this test + front-end default together)", ra, "60")
	}
	// Body must be machine-parseable JSON with the documented error key.
	// The front-end doesn't read this (it branches on status), but curl /
	// monitoring scripts do.
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if !strings.Contains(w.Body.String(), `"error":"too many attempts`) {
		t.Errorf("body = %q, want JSON with error 'too many attempts...'", w.Body.String())
	}
}
