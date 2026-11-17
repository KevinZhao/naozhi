package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestIsAuthenticated_NoTokenRefusesReboundHost: without a dashboard token a
// page on evil.example, its DNS rebound to 127.0.0.1, is same-origin with the
// dashboard, so the Host it sends is the only thing telling it apart. Only a
// name rebinding cannot take over (an IP literal, localhost) is let in.
func TestIsAuthenticated_NoTokenRefusesReboundHost(t *testing.T) {
	t.Parallel()
	a := &Handlers{DashboardToken: ""}
	for host, want := range map[string]bool{
		"127.0.0.1:8080":      true,
		"127.0.0.1":           true,
		"[::1]:8080":          true,
		"192.168.1.20:8080":   true,
		"localhost:8080":      true,
		"LOCALHOST.:8080":     true,
		"app.localhost:8080":  true,
		"evil.example:8080":   false,
		"evil.example":        false,
		"localhost.evil:8080": false,
		"":                    false,
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
		r.Host = host
		if got := a.IsAuthenticated(r); got != want {
			t.Errorf("Host %q: IsAuthenticated = %v, want %v", host, got, want)
		}
	}
}

// TestRequireAuth_NoTokenReboundHostRefused: the API chain answers a rebound
// page's same-origin request with 401 instead of running the handler.
func TestRequireAuth_NoTokenReboundHostRefused(t *testing.T) {
	t.Parallel()
	a := &Handlers{DashboardToken: ""}
	called := false
	h := RequireSameOrigin(false)(a.RequireAuth(func(http.ResponseWriter, *http.Request) { called = true }))
	r := httptest.NewRequest(http.MethodPost, "http://evil.example:8080/api/sessions/send", nil)
	r.Header.Set("Origin", "http://evil.example:8080")
	rec := httptest.NewRecorder()
	h(rec, r)
	if called || rec.Code != http.StatusUnauthorized {
		t.Fatalf("rebound request: handler called=%v, code=%d; want refused with 401", called, rec.Code)
	}
}
