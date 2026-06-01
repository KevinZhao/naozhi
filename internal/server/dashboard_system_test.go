package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleSystemDaemons_DisabledReturnsEmptyArray(t *testing.T) {
	t.Parallel()
	srv := newTestServer(&mockPlatform{})
	// SysessionManager intentionally nil — test the disabled-path
	// contract (must still return valid JSON array, not 404).

	r := httptest.NewRequest(http.MethodGet, "/api/system/daemons", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", w.Code, w.Body.String())
	}
	body := strings.TrimSpace(w.Body.String())
	if body != "[]" {
		t.Errorf("body = %q, want %q", body, "[]")
	}
}

func TestHandleClearLabelOrigin_RequiresKey(t *testing.T) {
	t.Parallel()
	srv := newTestServer(&mockPlatform{})

	cases := []struct {
		name string
		body string
		want int
	}{
		{"empty body", "", http.StatusBadRequest},
		{"missing key field", `{}`, http.StatusBadRequest},
		{"empty key", `{"key":""}`, http.StatusBadRequest},
		{"reserved (cron) key rejected", `{"key":"cron:foo"}`, http.StatusBadRequest},
		{"reserved (sys) key rejected", `{"key":"sys:auto-titler"}`, http.StatusBadRequest},
		{"reserved (project) key rejected", `{"key":"project:foo:planner"}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodPost,
				"/api/system/labels/clear-origin",
				strings.NewReader(c.body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.mux.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Errorf("status = %d, want %d; body=%q", w.Code, c.want, w.Body.String())
			}
		})
	}
}

func TestHandleClearLabelOrigin_UnknownKeyReturns404(t *testing.T) {
	t.Parallel()
	srv := newTestServer(&mockPlatform{})

	body := `{"key":"feishu:direct:nobody:general"}`
	r := httptest.NewRequest(http.MethodPost,
		"/api/system/labels/clear-origin", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%q", w.Code, w.Body.String())
	}
}

// TestHandleClearLabelOrigin_ErrorEnvelopeShape pins R247-ARCH-3 (#612):
// the error paths of this /api/* handler now emit the unified errEnvelope
// JSON ({"error":..,"code":..}) with a JSON Content-Type, matching the rest
// of the package — not the legacy text/plain http.Error the front-end
// couldn't parse via body.error. Covers the 400 (bad-request) and 404
// (unknown-key) branches.
func TestHandleClearLabelOrigin_ErrorEnvelopeShape(t *testing.T) {
	t.Parallel()
	srv := newTestServer(&mockPlatform{})

	cases := []struct {
		name     string
		body     string
		wantCode int
		wantTok  string // stable machine-readable code in the envelope
	}{
		{"missing key", `{}`, http.StatusBadRequest, "missing_key"},
		{"reserved namespace", `{"key":"cron:foo"}`, http.StatusBadRequest, "reserved_namespace"},
		{"unknown key", `{"key":"feishu:direct:nobody:general"}`, http.StatusNotFound, "not_found"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodPost,
				"/api/system/labels/clear-origin", strings.NewReader(c.body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.mux.ServeHTTP(w, r)

			if w.Code != c.wantCode {
				t.Fatalf("status = %d, want %d; body=%q", w.Code, c.wantCode, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("Content-Type = %q, want application/json (text/plain http.Error not migrated)", ct)
			}
			var env struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("error body is not JSON envelope: %v; body=%q", err, w.Body.String())
			}
			if env.Error == "" {
				t.Errorf("envelope.error empty; body=%q", w.Body.String())
			}
			if env.Code != c.wantTok {
				t.Errorf("envelope.code = %q, want %q", env.Code, c.wantTok)
			}
		})
	}
}

// TestHandleSystemDaemons_JSONShape sanity-checks the top-level shape
// when sysession is wired:  response is a JSON array (possibly empty
// in unconfigured tests), each element has the documented field set.
// We don't construct a real Manager here — that's exercised by the
// sysession package tests directly.  This handler test only locks the
// HTTP-layer contract.
func TestHandleSystemDaemons_JSONShape(t *testing.T) {
	t.Parallel()
	srv := newTestServer(&mockPlatform{})

	r := httptest.NewRequest(http.MethodGet, "/api/system/daemons", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)

	var got []map[string]any
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode body: %v; body=%q", err, w.Body.String())
	}
	// Disabled Manager → empty list.  Asserting len here keeps the
	// test green even if a future contributor wires a default daemon
	// — they'd then need to update both this assertion and the
	// disabled-path assumption.
	if len(got) != 0 {
		t.Errorf("disabled handler: expected empty list, got %v", got)
	}
}
