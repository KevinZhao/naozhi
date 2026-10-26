package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestStats_VersionTagPresent pins the /api/sessions stats contract: when
// ServerOptions.Version is set, `stats.version_tag` carries the same value
// so dashboard.js can render the footer. The existing `version` uint64
// field (store mutation counter) must remain untouched.
func TestStats_VersionTagPresent(t *testing.T) {
	router := session.NewRouter(session.RouterConfig{})
	srv := NewWithOptions(ServerOptions{
		Addr:    ":0",
		Router:  router,
		Backend: "claude",
		Identity: IdentityOptions{
			Version: "v9.9.9-test",
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.sessionH.HandleList(w, req)

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	stats, ok := resp["stats"].(map[string]any)
	if !ok {
		t.Fatalf("stats wrong type: %T", resp["stats"])
	}
	got, ok := stats["version_tag"]
	if !ok {
		t.Fatalf("stats.version_tag missing; body=%s", w.Body.String())
	}
	if got != "v9.9.9-test" {
		t.Errorf("stats.version_tag = %v, want v9.9.9-test", got)
	}
	// The legacy `version` uint64 (store-mutation counter) must still be
	// present and numeric — a string-shaped value would break dashboard.js
	// consumers that treat it as a monotonic integer.
	ver, ok := stats["version"]
	if !ok {
		t.Fatal("stats.version (uint64 counter) missing — version_tag collision regression")
	}
	if _, ok := ver.(float64); !ok {
		t.Errorf("stats.version should decode as number, got %T %v", ver, ver)
	}
}

// TestStats_VersionTagOmittedWhenUnset regresses the omitempty contract on
// the stats sub-object. dashboard.js guards with `if (tag)` so an empty
// string works too, but the wire stability test pins the absent-key shape.
func TestStats_VersionTagOmittedWhenUnset(t *testing.T) {
	router := session.NewRouter(session.RouterConfig{})
	srv := NewWithOptions(ServerOptions{
		Addr:    ":0",
		Router:  router,
		Backend: "claude",
		// Version empty
	})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.sessionH.HandleList(w, req)

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	stats := resp["stats"].(map[string]any)
	if v, ok := stats["version_tag"]; ok {
		t.Errorf("stats.version_tag should be absent when unset, got %v", v)
	}
}
