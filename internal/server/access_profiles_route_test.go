package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestHandleAccessProfiles_ShapeAndNoLeak drives GET /api/access-profiles end
// to end and asserts (a) the JSON shape the dashboard consumes and (b) the
// security invariant that env values / secrets NEVER appear in the response.
func TestHandleAccessProfiles_ShapeAndNoLeak(t *testing.T) {
	router := session.NewRouter(session.RouterConfig{
		Workspace: t.TempDir(),
		AccessProfiles: map[string]session.AccessProfile{
			"bedrock-opus": {
				DisplayName:  "Bedrock · Opus 4.8",
				ChipColor:    "#7c5cff",
				DefaultModel: "claude-opus-4-8",
				Env: map[string]string{
					"CLAUDE_CODE_USE_BEDROCK":    "1",
					"ANTHROPIC_BEDROCK_BASE_URL": "http://127.0.0.1:8889",
				},
			},
		},
	})
	srv := NewWithOptions(ServerOptions{Addr: ":0", Router: router, Backend: "claude"})
	srv.registerDashboard()

	req := httptest.NewRequest(http.MethodGet, "/api/access-profiles", nil)
	w := httptest.NewRecorder()
	srv.accessProfilesH.HandleList(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()

	// Security: NO env key or value may appear in the payload.
	for _, forbidden := range []string{
		"CLAUDE_CODE_USE_BEDROCK", "ANTHROPIC_BEDROCK_BASE_URL", "127.0.0.1:8889", "\"env\"",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("response leaked env content %q: %s", forbidden, body)
		}
	}

	var resp struct {
		Profiles []session.AccessProfileInfo `json:"profiles"`
		Default  string                      `json:"default"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Profiles) != 1 {
		t.Fatalf("want 1 profile, got %d", len(resp.Profiles))
	}
	p := resp.Profiles[0]
	if p.ID != "bedrock-opus" || p.DisplayName != "Bedrock · Opus 4.8" ||
		p.ChipColor != "#7c5cff" || p.DefaultModel != "claude-opus-4-8" {
		t.Errorf("profile fields wrong: %+v", p)
	}
	if !p.SecretOK {
		t.Error("no *_FILE reference → SecretOK should be true")
	}
}

// TestHandleAccessProfiles_DefaultSurfaced: the configured
// default_access_profile is echoed in the `default` field so the new-session
// picker can pre-select it. Also asserts no env leak on this path.
func TestHandleAccessProfiles_DefaultSurfaced(t *testing.T) {
	router := session.NewRouter(session.RouterConfig{
		Workspace:            t.TempDir(),
		DefaultAccessProfile: "1p",
		AccessProfiles: map[string]session.AccessProfile{
			"1p": {
				DisplayName:  "个人 Anthropic",
				DefaultModel: "claude-opus-4-8",
				Env:          map[string]string{"CLAUDE_CODE_USE_BEDROCK": "0"},
			},
			"bedrock": {
				DisplayName: "公司 Bedrock",
				Env:         map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"},
			},
		},
	})
	srv := NewWithOptions(ServerOptions{Addr: ":0", Router: router, Backend: "claude"})
	srv.registerDashboard()

	req := httptest.NewRequest(http.MethodGet, "/api/access-profiles", nil)
	w := httptest.NewRecorder()
	srv.accessProfilesH.HandleList(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp struct {
		Profiles []session.AccessProfileInfo `json:"profiles"`
		Default  string                      `json:"default"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Default != "1p" {
		t.Errorf("default = %q, want %q", resp.Default, "1p")
	}
	// The default profile id is a non-sensitive value, but env must still not leak.
	if strings.Contains(w.Body.String(), "CLAUDE_CODE_USE_BEDROCK") {
		t.Errorf("response leaked env: %s", w.Body.String())
	}
}

// TestHandleAccessProfiles_EmptyRegistry: single-auth deployments return an
// empty (non-null) profiles array so the dashboard JS Array.isArray check
// passes and the picker/chip simply stay hidden.
func TestHandleAccessProfiles_EmptyRegistry(t *testing.T) {
	router := session.NewRouter(session.RouterConfig{Workspace: t.TempDir()})
	srv := NewWithOptions(ServerOptions{Addr: ":0", Router: router, Backend: "claude"})
	srv.registerDashboard()

	req := httptest.NewRequest(http.MethodGet, "/api/access-profiles", nil)
	w := httptest.NewRecorder()
	srv.accessProfilesH.HandleList(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"profiles":[]`) {
		t.Errorf("empty registry should emit []; got %s", w.Body.String())
	}
}
