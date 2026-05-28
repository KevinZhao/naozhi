package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleLogin_TrustedProxyRequiresXFF pins R247-SEC-25 (#528): when
// TrustedProxy=true the login handler must reject (400) requests that
// arrive without a parseable X-Forwarded-For tail, instead of silently
// falling back to r.RemoteAddr — which under ALB/CloudFront is the
// proxy's single IP and would collapse every XFF-less caller into one
// loginLimiter bucket. A single attacker bypassing the proxy could
// otherwise burn that shared bucket and 429-starve every other XFF-less
// probe, defeating per-IP brute-force defence.
//
// Non-trusted-proxy mode is unchanged: r.RemoteAddr is the legitimate
// client IP and serves as a per-IP key without any operator misconfig
// implication.
func TestHandleLogin_TrustedProxyRequiresXFF(t *testing.T) {
	t.Parallel()

	type tc struct {
		name         string
		TrustedProxy bool
		xff          string
		// wantStatus is the response status code we expect. 400 means the
		// XFF gate fired before any auth comparison; 401 means auth ran
		// (so the gate did NOT fire) and rejected the wrong token.
		wantStatus int
	}
	cases := []tc{
		{
			name:         "trustedProxy_no_xff_rejects_400",
			TrustedProxy: true,
			xff:          "",
			wantStatus:   http.StatusBadRequest,
		},
		{
			name:         "trustedProxy_unparseable_xff_rejects_400",
			TrustedProxy: true,
			xff:          "garbage,not-an-ip",
			wantStatus:   http.StatusBadRequest,
		},
		{
			name:         "trustedProxy_valid_xff_proceeds_to_auth",
			TrustedProxy: true,
			xff:          "203.0.113.7",
			wantStatus:   http.StatusUnauthorized,
		},
		{
			name:         "no_trustedProxy_no_xff_proceeds_to_auth",
			TrustedProxy: false,
			xff:          "",
			wantStatus:   http.StatusUnauthorized,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Handlers{
				DashboardToken: "secret",
				cookieSecret:   []byte("cookie"),
				loginLimiter:   NewLoginLimiter(),
				TrustedProxy:   c.TrustedProxy,
			}
			body := `{"token":"wrong"}`
			r := httptest.NewRequest(http.MethodPost,
				"http://naozhi.example/api/auth/login",
				strings.NewReader(body))
			r.Host = "naozhi.example"
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", "http://naozhi.example")
			r.RemoteAddr = "10.0.0.1:54321" // proxy's address in TrustedProxy mode
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			w := httptest.NewRecorder()
			a.HandleLogin(w, r)
			if w.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d (body=%q)",
					w.Code, c.wantStatus, strings.TrimSpace(w.Body.String()))
			}
			// The 400 path must NOT consume a loginLimiter token — otherwise
			// the misconfig path would still let an attacker burn the
			// limiter slot. We can't read the limiter directly, but we can
			// confirm the response body carries the explicit XFF complaint
			// rather than the rate-limit "too many attempts" payload.
			if c.wantStatus == http.StatusBadRequest {
				if !strings.Contains(w.Body.String(), "X-Forwarded-For") {
					t.Errorf("body should reference X-Forwarded-For, got %q", w.Body.String())
				}
			}
		})
	}
}
