package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// #2433 item 8: HandleLogin's bad-request reply carries a JSON body, so it
// must also be labelled application/json (http.Error forced text/plain).
func TestHandleLogin_BadRequestIsJSON(t *testing.T) {
	t.Parallel()
	a := &Handlers{
		DashboardToken: "secret",
		cookieSecret:   []byte("cookie"),
		loginLimiter:   NewLoginLimiter(),
	}
	r := httptest.NewRequest(http.MethodPost, "http://naozhi.example/api/auth/login",
		strings.NewReader(`{"token": not-json`))
	r.Host = "naozhi.example"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://naozhi.example")
	r.RemoteAddr = "10.0.0.1:54321"
	w := httptest.NewRecorder()
	a.HandleLogin(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, w.Body.String())
	}
	if resp["error"] != "bad request" {
		t.Fatalf("error = %q, want %q", resp["error"], "bad request")
	}
}
