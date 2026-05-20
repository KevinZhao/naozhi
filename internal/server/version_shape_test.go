package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

// TestHealth_VersionAbsentOnUnauthedProbe pins R229-SEC-7: the build tag is
// no longer exposed to unauthenticated probes. Public /health must only
// return status + uptime so an attacker on the open internet cannot
// fingerprint the running binary version. Authenticated probes still get
// the version via the embedded auth section (covered by
// TestHealth_VersionPresentOnAuthedProbe below).
func TestHealth_VersionAbsentOnUnauthedProbe(t *testing.T) {
	router := session.NewRouter(session.RouterConfig{})
	platforms := map[string]platform.Platform{"test": &mockPlatform{}}
	srv := New(":0", router, platforms, nil, nil, nil, "claude", ServerOptions{
		DashboardToken: "secret",
		Version:        "v1.2.3-test",
	})
	srv.registerDashboard()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.healthH.handleHealth(w, req)

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if v, ok := body["version"]; ok {
		t.Errorf("version leaked to unauthed /health; got %v body=%s", v, w.Body.String())
	}
	// Unauthenticated probe must only carry status + uptime — anything else
	// indicates the auth embed regressed and is leaking infrastructure
	// topology.
	allowed := map[string]bool{"status": true, "uptime": true}
	for k := range body {
		if !allowed[k] {
			t.Errorf("unauthed /health leaked %q (body=%s)", k, w.Body.String())
		}
	}
}

// TestHealth_VersionPresentOnAuthedProbe pins that the build tag still
// reaches authenticated probes (operators with a valid dashboard token).
// R229-SEC-7 only restricted the unauthed surface; auth flow must keep
// returning version so the dashboard footer continues to render.
func TestHealth_VersionPresentOnAuthedProbe(t *testing.T) {
	router := session.NewRouter(session.RouterConfig{})
	platforms := map[string]platform.Platform{"test": &mockPlatform{}}
	srv := New(":0", router, platforms, nil, nil, nil, "claude", ServerOptions{
		DashboardToken: "secret",
		Version:        "v1.2.3-test",
	})
	srv.registerDashboard()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	srv.healthH.handleHealth(w, req)

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	got, ok := body["version"]
	if !ok {
		t.Fatalf("version missing from authed /health; body=%s", w.Body.String())
	}
	if got != "v1.2.3-test" {
		t.Errorf("version = %v, want v1.2.3-test", got)
	}
}

// TestStats_VersionTagPresent pins the /api/sessions stats contract: when
// ServerOptions.Version is set, `stats.version_tag` carries the same value
// so dashboard.js can render the footer. The existing `version` uint64
// field (store mutation counter) must remain untouched.
func TestStats_VersionTagPresent(t *testing.T) {
	router := session.NewRouter(session.RouterConfig{})
	srv := New(":0", router, nil, nil, nil, nil, "claude", ServerOptions{
		Version: "v9.9.9-test",
	})
	srv.registerDashboard()

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.sessionH.handleList(w, req)

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
	srv := New(":0", router, nil, nil, nil, nil, "claude", ServerOptions{
		// Version empty
	})
	srv.registerDashboard()

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.sessionH.handleList(w, req)

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	stats := resp["stats"].(map[string]any)
	if v, ok := stats["version_tag"]; ok {
		t.Errorf("stats.version_tag should be absent when unset, got %v", v)
	}
}
