package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

func newCreateProfileServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("server:\n  addr: \"127.0.0.1:8080\"\ncli:\n  backend: claude\n  path: /usr/bin/claude\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secretsDir := filepath.Join(dir, "secrets")
	router := session.NewRouter(session.RouterConfig{Workspace: dir})
	srv := NewWithOptions(ServerOptions{
		Addr:                    ":0",
		Router:                  router,
		Backend:                 "claude",
		ConfigPath:              cfgPath,
		AccessProfileSecretsDir: secretsDir,
	})
	srv.registerDashboard()
	return srv, cfgPath, secretsDir
}

func postCreateProfile(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/access-profiles", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleCreateAccessProfile(w, req)
	return w
}

func TestCreateAccessProfile_BedrockNoSecret(t *testing.T) {
	srv, cfgPath, _ := newCreateProfileServer(t)
	body := `{"id":"bedrock-opus","display_name":"Bedrock · Opus","chip_color":"#7c5cff","default_model":"claude-opus-4-8","env":{"CLAUDE_CODE_USE_BEDROCK":"1","ANTHROPIC_BEDROCK_BASE_URL":"http://127.0.0.1:8889"}}`
	w := postCreateProfile(t, srv, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	// Live registry updated (no restart).
	if !srv.router.HasAccessProfile("bedrock-opus") {
		t.Error("profile not live after create")
	}
	// config.yaml written.
	data, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(data), "bedrock-opus") {
		t.Error("config.yaml missing new profile")
	}
}

func TestCreateAccessProfile_WithSecretFile(t *testing.T) {
	srv, cfgPath, secretsDir := newCreateProfileServer(t)
	body := `{"id":"1p-fable","display_name":"1P · Fable","default_model":"claude-fable-5","env":{"CLAUDE_CODE_USE_BEDROCK":"0","ANTHROPIC_BASE_URL":"https://api.anthropic.com"},"token_env_key":"ANTHROPIC_AUTH_TOKEN_FILE","token_content":"sk-secret-xyz"}`
	w := postCreateProfile(t, srv, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	// Secret written 0600.
	secretPath := filepath.Join(secretsDir, "1p-fable.token")
	info, err := os.Stat(secretPath)
	if err != nil {
		t.Fatalf("secret file not written: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("secret mode = %o, want 0600", info.Mode().Perm())
	}
	sd, _ := os.ReadFile(secretPath)
	if string(sd) != "sk-secret-xyz" {
		t.Errorf("secret content = %q", sd)
	}
	// config.yaml references the *_FILE path, NOT the token itself.
	cd, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(cd), "sk-secret-xyz") {
		t.Error("SECURITY: token leaked into config.yaml")
	}
	if !strings.Contains(string(cd), "ANTHROPIC_AUTH_TOKEN_FILE") {
		t.Error("config.yaml missing token file reference")
	}
	// Response never echoes the token.
	if strings.Contains(w.Body.String(), "sk-secret-xyz") {
		t.Error("SECURITY: token echoed in response")
	}
}

func TestCreateAccessProfile_Rejections(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"bad id", `{"id":"-bad","env":{}}`, http.StatusBadRequest},
		{"non-overlay env key", `{"id":"x","env":{"AWS_PROFILE":"admin"}}`, http.StatusBadRequest},
		{"SSRF base url", `{"id":"y","env":{"ANTHROPIC_BASE_URL":"http://169.254.169.254"}}`, http.StatusBadRequest},
		{"token key not *_FILE", `{"id":"z","env":{},"token_env_key":"ANTHROPIC_AUTH_TOKEN","token_content":"x"}`, http.StatusBadRequest},
		{"unknown default_backend", `{"id":"w","default_backend":"ghost","env":{}}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := newCreateProfileServer(t)
			w := postCreateProfile(t, srv, tc.body)
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestCreateAccessProfile_Duplicate(t *testing.T) {
	srv, _, _ := newCreateProfileServer(t)
	body := `{"id":"dup","display_name":"D","env":{}}`
	if w := postCreateProfile(t, srv, body); w.Code != http.StatusOK {
		t.Fatalf("first create failed: %d %s", w.Code, w.Body.String())
	}
	if w := postCreateProfile(t, srv, body); w.Code != http.StatusConflict {
		t.Errorf("duplicate status = %d, want 409", w.Code)
	}
}

func TestCreateAccessProfile_DisabledWithoutConfigPath(t *testing.T) {
	dir := t.TempDir()
	router := session.NewRouter(session.RouterConfig{Workspace: dir})
	srv := NewWithOptions(ServerOptions{Addr: ":0", Router: router, Backend: "claude"})
	srv.registerDashboard()
	w := postCreateProfile(t, srv, `{"id":"x","env":{}}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (disabled)", w.Code)
	}
}
