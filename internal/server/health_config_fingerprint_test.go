package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHandleHealth_ConfigFingerprint pins the #2538 contract: the loaded
// config's fingerprint (sha256 / loaded_at / path) rides the AUTHENTICATED
// /health section only — an anonymous probe must never see the hash or the
// path.
func TestHandleHealth_ConfigFingerprint(t *testing.T) {
	srv := newTestServerWithToken(&mockPlatform{}, "secret")
	srv.healthH.configSHA256 = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	srv.healthH.configLoadedAt = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	srv.healthH.configPath = "/etc/naozhi/config.yaml"

	authed := httptest.NewRequest(http.MethodGet, "/health", nil)
	authed.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	srv.healthH.handleHealth(w, authed)

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, w.Body.String())
	}
	if got := body["config_sha256"]; got != srv.healthH.configSHA256 {
		t.Errorf("config_sha256 = %v, want the full hash", got)
	}
	if got := body["config_loaded_at"]; got != "2026-09-05T12:00:00Z" {
		t.Errorf("config_loaded_at = %v, want RFC3339", got)
	}
	if got := body["config_path"]; got != "/etc/naozhi/config.yaml" {
		t.Errorf("config_path = %v", got)
	}

	anon := httptest.NewRequest(http.MethodGet, "/health", nil)
	w2 := httptest.NewRecorder()
	srv.healthH.handleHealth(w2, anon)
	if s := w2.Body.String(); strings.Contains(s, "config_sha256") || strings.Contains(s, "config.yaml") {
		t.Errorf("unauthenticated /health leaked the config fingerprint: %s", s)
	}
}
