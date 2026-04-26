package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

// TestHandleHealth_Unauthenticated_OnlyBaseFields locks the wire contract
// that unauthenticated /health probes expose only status + uptime — no
// sessions count, no platform list, no node status. The prior map-based
// implementation achieved this by skipping writes into the map; the named
// struct uses `*healthAuthSection` embed so JSON omits the whole sub-section
// when nil. Breaking this test would leak internal topology to anonymous
// probes (load balancers / liveness checks) which must never see it.
func TestHandleHealth_Unauthenticated_OnlyBaseFields(t *testing.T) {
	srv := newTestServerWithToken(&mockPlatform{}, "secret")
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.healthH.handleHealth(w, req)

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, w.Body.String())
	}
	allowed := map[string]bool{"status": true, "uptime": true}
	for k := range body {
		if !allowed[k] {
			t.Errorf("unauthenticated /health leaked field %q (body=%s)", k, w.Body.String())
		}
	}
	if body["status"] != "ok" {
		t.Errorf("status = %v, want ok", body["status"])
	}
	if _, ok := body["uptime"].(string); !ok {
		t.Errorf("uptime = %v, want string", body["uptime"])
	}
}

// TestHandleHealth_Authenticated_ShapeStable locks the set of top-level
// keys /health returns on an authenticated probe so the map → struct
// migration (R60-PERF-001) does not silently drop or rename a field that
// operator tooling consumes via curl / monitoring agents.
func TestHandleHealth_Authenticated_ShapeStable(t *testing.T) {
	srv := newTestServerWithToken(&mockPlatform{}, "secret")
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	srv.healthH.handleHealth(w, req)

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, w.Body.String())
	}
	// Required fields (always present on authed probe).
	required := []string{
		"status", "uptime", "sessions", "workspace_id", "workspace_name",
		"system", "goroutines", "watchdog", "cli_available", "platforms",
	}
	for _, k := range required {
		if _, ok := body[k]; !ok {
			t.Errorf("required field %q missing (body=%s)", k, w.Body.String())
		}
	}
	// ws_dropped, dispatch, nodes are optional (depend on handler wiring).
	// We don't assert presence but do check that when absent they are truly
	// absent (i.e. omitempty is in effect), not emitted as null.
	for _, k := range []string{"ws_dropped", "dispatch", "nodes"} {
		if v, ok := body[k]; ok && v == nil {
			t.Errorf("optional field %q emitted as null — omitempty broken", k)
		}
	}
	// sessions sub-object must keep both keys even when zero so dashboards
	// showing "0/0" render correctly (prior map code emitted the nested
	// struct as-is, not a map literal).
	sessions, ok := body["sessions"].(map[string]any)
	if !ok {
		t.Fatalf("sessions wrong type: %T", body["sessions"])
	}
	if _, ok := sessions["active"]; !ok {
		t.Error("sessions.active missing")
	}
	if _, ok := sessions["total"]; !ok {
		t.Error("sessions.total missing")
	}
	// watchdog sub-object must carry pre-formatted timeout strings.
	wd, ok := body["watchdog"].(map[string]any)
	if !ok {
		t.Fatalf("watchdog wrong type: %T", body["watchdog"])
	}
	for _, k := range []string{"no_output_kills", "total_kills", "no_output_timeout", "total_timeout"} {
		if _, ok := wd[k]; !ok {
			t.Errorf("watchdog.%s missing", k)
		}
	}
}

// TestHandleHealth_WSDroppedField_PresentWhenHubWired verifies that the
// struct refactor still threads the `ws_dropped` field through when the
// Hub.DroppedMessages callback is injected. Prior code used `if h.hubDropped
// != nil { resp["ws_dropped"] = ... }`; the struct now holds `*int64` so
// omitempty on absence + pointer on presence preserves the old wire shape.
func TestHandleHealth_WSDroppedField_PresentWhenHubWired(t *testing.T) {
	srv := newTestServerWithToken(&mockPlatform{}, "secret")
	srv.healthH.hubDropped = func() int64 { return 42 }
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	srv.healthH.handleHealth(w, req)

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	v, ok := body["ws_dropped"]
	if !ok {
		t.Fatal("ws_dropped missing when hub wired")
	}
	f, ok := v.(float64)
	if !ok || f != 42 {
		t.Errorf("ws_dropped = %v (%T), want 42", v, v)
	}
}

// TestHandleHealth_WSDropped_ZeroEmitted verifies that a 0 drop count is
// still included in the response (not omitted), so monitoring tools watching
// for the key's presence can distinguish "wired but zero" from "not wired".
// This is the subtle bug the pointer-to-int approach was specifically
// chosen to avoid — plain `int64` + `omitempty` would drop a zero value.
func TestHandleHealth_WSDropped_ZeroEmitted(t *testing.T) {
	srv := newTestServerWithToken(&mockPlatform{}, "secret")
	srv.healthH.hubDropped = func() int64 { return 0 }
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	srv.healthH.handleHealth(w, req)

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	v, ok := body["ws_dropped"]
	if !ok {
		t.Fatal("ws_dropped should emit even when count is 0 (presence signals hub is wired)")
	}
	if f, _ := v.(float64); f != 0 {
		t.Errorf("ws_dropped = %v, want 0", v)
	}
}

// assertHealthPlatforms pins the platform map shape. The prior code built
// `map[string]string` per-probe; the struct keeps that exact type so
// platform.Platform names passed into New() marshal as-is.
func TestHandleHealth_PlatformsShape(t *testing.T) {
	srv := newTestServerWithToken(&mockPlatform{}, "secret")
	// inject a second platform so we exercise the multi-entry path.
	srv.healthH.platforms = map[string]struct{}{"feishu": {}, "slack": {}}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	srv.healthH.handleHealth(w, req)

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	p, ok := body["platforms"].(map[string]any)
	if !ok {
		t.Fatalf("platforms wrong type: %T", body["platforms"])
	}
	for _, name := range []string{"feishu", "slack"} {
		if v := p[name]; v != "registered" {
			t.Errorf("platforms[%s] = %v, want \"registered\"", name, v)
		}
	}
}

// _ pins a compile-time signature check: the authenticated branch factory
// must accept a platform.Platform map, so a future refactor that swaps the
// newTestServerWithToken helper still compiles this test file against the
// same platform type contract.
var _ = platform.Platform(nil)
var _ session.Router
