package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
	_, hs := newTestServerWithTokenHS(&mockPlatform{}, "secret")
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	hs.healthH.handleHealth(w, req)

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
	_, hs := newTestServerWithTokenHS(&mockPlatform{}, "secret")
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	hs.healthH.handleHealth(w, req)

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
	_, hs := newTestServerWithTokenHS(&mockPlatform{}, "secret")
	hs.healthH.hubDropped = func() int64 { return 42 }
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	hs.healthH.handleHealth(w, req)

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
	_, hs := newTestServerWithTokenHS(&mockPlatform{}, "secret")
	hs.healthH.hubDropped = func() int64 { return 0 }
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	hs.healthH.handleHealth(w, req)

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

// connReportingPlatform is a mockPlatform that answers ConnStateReporter.
type connReportingPlatform struct {
	mockPlatform
	state platform.ConnState
	ok    bool
}

func (c *connReportingPlatform) ConnState() (platform.ConnState, bool) { return c.state, c.ok }

// healthBody GETs an authenticated /health and decodes it.
func healthBody(t *testing.T, hs *handlerSet) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	hs.healthH.handleHealth(w, req)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// TestHandleHealth_PlatformsShape: `platforms` stays a name -> string map (the
// shape doctor decodes), "registered" for adapters that cannot observe their
// connection, and with no reporter at all platform_conn is omitted.
func TestHandleHealth_PlatformsShape(t *testing.T) {
	_, hs := newTestServerWithTokenHS(&mockPlatform{}, "secret")
	hs.healthH.platforms = map[string]platform.Platform{
		"feishu": &mockPlatform{},
		"slack":  &connReportingPlatform{ok: false},
	}

	body := healthBody(t, hs)
	p, ok := body["platforms"].(map[string]any)
	if !ok {
		t.Fatalf("platforms wrong type: %T", body["platforms"])
	}
	if len(p) != 2 {
		t.Errorf("platforms = %v, want both registered names", p)
	}
	for _, name := range []string{"feishu", "slack"} {
		if v := p[name]; v != "registered" {
			t.Errorf("platforms[%s] = %v, want \"registered\"", name, v)
		}
	}
	if v, present := body["platform_conn"]; present {
		t.Errorf("platform_conn = %v, want omitted when no platform reports a state", v)
	}
}

// TestHandleHealth_PlatformsEmptyIsObject: with no platform configured the
// field is still `{}`, never null, which is what the doctor contract fixture
// and any `platforms | keys` consumer expect.
func TestHandleHealth_PlatformsEmptyIsObject(t *testing.T) {
	_, hs := newTestServerWithTokenHS(&mockPlatform{}, "secret")
	hs.healthH.platforms = nil

	p, ok := healthBody(t, hs)["platforms"].(map[string]any)
	if !ok || len(p) != 0 {
		t.Errorf("platforms = %#v, want an empty object", p)
	}
}

// TestHandleHealth_PlatformsServeLiveConnState: a reporting adapter's state is
// read per request, not frozen at construction, and its detail lands in
// platform_conn next to the non-reporting "registered" entry.
func TestHandleHealth_PlatformsServeLiveConnState(t *testing.T) {
	_, hs := newTestServerWithTokenHS(&mockPlatform{}, "secret")
	since := time.Now().Add(-90 * time.Second)
	errAt := time.Now().Add(-30 * time.Second)
	slack := &connReportingPlatform{ok: true, state: platform.ConnState{
		State: platform.ConnDisconnected, Since: since,
		LastError: "read tcp: connection reset", LastErrorAt: errAt,
	}}
	hs.healthH.platforms = map[string]platform.Platform{
		"feishu": &mockPlatform{},
		"slack":  slack,
	}

	body := healthBody(t, hs)
	p, _ := body["platforms"].(map[string]any)
	if p["slack"] != "disconnected" || p["feishu"] != "registered" {
		t.Fatalf("platforms = %v, want slack=disconnected feishu=registered", p)
	}
	conn, _ := body["platform_conn"].(map[string]any)
	if _, has := conn["feishu"]; has || len(conn) != 1 {
		t.Fatalf("platform_conn = %v, want only the reporting platform", conn)
	}
	got, _ := conn["slack"].(map[string]any)
	want := map[string]any{
		"state":         "disconnected",
		"since":         since.UTC().Format(time.RFC3339),
		"last_error":    "read tcp: connection reset",
		"last_error_at": errAt.UTC().Format(time.RFC3339),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("platform_conn.slack.%s = %v, want %v", k, got[k], v)
		}
	}
	if ago, _ := got["since_ago"].(string); ago != "1m30s" && ago != "1m31s" {
		t.Errorf("since_ago = %q, want ~1m30s", ago)
	}

	slack.state = platform.ConnState{State: platform.ConnConnected, Since: time.Now()}
	body = healthBody(t, hs)
	if p, _ := body["platforms"].(map[string]any); p["slack"] != "connected" {
		t.Errorf("after reconnect platforms.slack = %v, want connected (state must be read per request)", p["slack"])
	}
	conn, _ = body["platform_conn"].(map[string]any)
	if got, _ := conn["slack"].(map[string]any); got["last_error"] != nil || got["last_error_at"] != nil {
		t.Errorf("platform_conn.slack = %v, want no error fields when none recorded", got)
	}

	slack.state = platform.ConnState{State: platform.ConnConnecting}
	conn, _ = healthBody(t, hs)["platform_conn"].(map[string]any)
	got, _ = conn["slack"].(map[string]any)
	if _, has := got["since"]; has || got["since_ago"] != nil || got["state"] != "connecting" {
		t.Errorf("platform_conn.slack = %v, want state and no since fields for a zero Since", got)
	}
}

// TestHealthEndpoints_SecurityHeaders pins the R20260616-SEC-3 fix: /livez,
// /readyz, and /health (unauth + authed) all carry X-Content-Type-Options:
// nosniff and Cache-Control: no-store so a browser-renderable response can't be
// MIME-sniffed and a proxy/CDN can't cache a per-request health snapshot.
func TestHealthEndpoints_SecurityHeaders(t *testing.T) {
	assertHeaders := func(t *testing.T, w *httptest.ResponseRecorder) {
		t.Helper()
		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
	}

	t.Run("livez", func(t *testing.T) {
		_, hs := newTestServerWithTokenHS(&mockPlatform{}, "secret")
		w := httptest.NewRecorder()
		hs.healthH.handleLivez(w, httptest.NewRequest(http.MethodGet, "/livez", nil))
		assertHeaders(t, w)
	})
	t.Run("readyz", func(t *testing.T) {
		_, hs := newTestServerWithTokenHS(&mockPlatform{}, "secret")
		w := httptest.NewRecorder()
		hs.healthH.handleReadyz(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		assertHeaders(t, w)
	})
	t.Run("health_unauth", func(t *testing.T) {
		_, hs := newTestServerWithTokenHS(&mockPlatform{}, "secret")
		w := httptest.NewRecorder()
		hs.healthH.handleHealth(w, httptest.NewRequest(http.MethodGet, "/health", nil))
		assertHeaders(t, w)
	})
	t.Run("health_authed", func(t *testing.T) {
		_, hs := newTestServerWithTokenHS(&mockPlatform{}, "secret")
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		req.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		hs.healthH.handleHealth(w, req)
		assertHeaders(t, w)
	})
}

// _ pins a compile-time signature check: the authenticated branch factory
// must accept a platform.Platform map, so a future refactor that swaps the
// newTestServerWithToken helper still compiles this test file against the
// same platform type contract.
var _ = platform.Platform(nil)
var _ session.Router
