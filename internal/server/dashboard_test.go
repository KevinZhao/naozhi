package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ─── handleAPISessionEvents ──────────────────────────────────────────────────

func TestHandleAPISessionEvents_MissingKey(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/events", nil)
	w := httptest.NewRecorder()
	srv.handleAPISessionEvents(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "missing key") {
		t.Errorf("body = %q, want 'missing key'", w.Body.String())
	}
}

func TestHandleAPISessionEvents_SessionNotFound(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/events?key=no-such-key", nil)
	w := httptest.NewRecorder()
	srv.handleAPISessionEvents(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "session not found") {
		t.Errorf("body = %q, want 'session not found'", w.Body.String())
	}
}

// ─── handleAPISend ────────────────────────────────────────────────────────────

func TestHandleAPISend_MissingKeyJSON(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	body := `{"text":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "key is required") {
		t.Errorf("body = %q, want 'key is required'", w.Body.String())
	}
}

func TestHandleAPISend_MissingTextAndFiles(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	body := `{"key":"p:t:u:general"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "text or files") {
		t.Errorf("body = %q, want 'text or files'", w.Body.String())
	}
}

func TestHandleAPISend_InvalidJSON(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader("{bad json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleAPISend_UnauthorizedNoToken(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	srv.SetDashboardToken("secret")

	body := `{"key":"p:t:u:general","text":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandleAPISend_UnauthorizedWrongToken(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	srv.SetDashboardToken("secret")

	body := `{"key":"p:t:u:general","text":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandleAPISend_AcceptedWithValidToken(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	srv.SetDashboardToken("secret")

	body := `{"key":"p:t:u:general","text":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", w.Code)
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "accepted" {
		t.Errorf("status = %q, want accepted", resp["status"])
	}
	if resp["key"] != "p:t:u:general" {
		t.Errorf("key = %q, want p:t:u:general", resp["key"])
	}
}

func TestHandleAPISend_AcceptedNoAuth(t *testing.T) {
	srv := newTestServer(&mockPlatform{}) // no dashboardToken

	body := `{"key":"p:t:u:general","text":"hello world"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", w.Code)
	}
}

func TestHandleAPISend_ConflictWhenBusy(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	key := "p:t:u:general"

	// Manually acquire the session guard to simulate a busy session.
	srv.sessionGuard.TryAcquire(key)
	defer srv.sessionGuard.Release(key)

	body := `{"key":"p:t:u:general","text":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["error"] != "session busy" {
		t.Errorf("error = %q, want 'session busy'", resp["error"])
	}
}

func TestHandleAPISend_ResponseIsJSON(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	body := bytes.NewBufferString(`{"key":"x:y:z:general","text":"test"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}
