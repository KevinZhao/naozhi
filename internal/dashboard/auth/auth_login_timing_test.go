package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleLogin_NoTokenConfiguredRejects pins R220-SEC-2: when
// DashboardToken is empty (operator did not configure auth) the login
// endpoint must reject submissions with 401 just like the
// configured-but-wrong path. Previously the handler short-circuited
// via `if a.DashboardToken == "" || !matched` which let the empty-token
// branch return faster than the matched-evaluation branch — a remote
// timing probe could distinguish "no token configured" from "token
// configured, but wrong". The fix combines both checks via bitwise AND
// so neither branch is skipped, keeping latency symmetric.
//
// This test does NOT measure latency directly (Go's test scheduler is
// too noisy for sub-millisecond timing assertions); it pins the
// observable behavior — both paths reject — so any future refactor
// that re-introduces an early-out keeps the rejection invariant intact.
func TestHandleLogin_NoTokenConfiguredRejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		DashboardToken string
		submitted      string
		wantStatus     int
	}{
		{"no_token_configured_any_input_rejects", "", "anything", http.StatusUnauthorized},
		{"no_token_configured_empty_input_rejects", "", "", http.StatusUnauthorized},
		{"configured_token_wrong_input_rejects", "secret", "wrong", http.StatusUnauthorized},
		{"configured_token_correct_input_accepts", "secret", "secret", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Handlers{
				DashboardToken: tc.DashboardToken,
				cookieSecret:   []byte("cookie"),
				loginLimiter:   NewLoginLimiter(),
			}
			body := `{"token":"` + tc.submitted + `"}`
			r := httptest.NewRequest(http.MethodPost, "http://naozhi.example/api/auth/login",
				strings.NewReader(body))
			r.Host = "naozhi.example"
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			a.HandleLogin(w, r)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%q)",
					w.Code, tc.wantStatus, strings.TrimSpace(w.Body.String()))
			}
		})
	}
}
