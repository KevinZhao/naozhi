package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/config"
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
	srv.accessProfilesH.HandleCreate(w, req)
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

// TestCreateAccessProfile_ConcurrentWritesAllPersist locks the [PR#2360 review
// HIGH] fix: N simultaneous creates with distinct ids must ALL survive in
// config.yaml (the read-modify-write is serialized by the handler's writeMu).
// Without the mutex, interleaved snapshots drop all but the last writer from
// disk while the live registry kept them — a divergence surfacing on restart.
func TestCreateAccessProfile_ConcurrentWritesAllPersist(t *testing.T) {
	srv, cfgPath, _ := newCreateProfileServer(t)
	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "p" + strconv.Itoa(i)
			body := `{"id":"` + id + `","display_name":"P` + strconv.Itoa(i) + `","env":{}}`
			w := postCreateProfile(t, srv, body)
			if w.Code != http.StatusOK {
				t.Errorf("create %s: status %d, body %s", id, w.Code, w.Body.String())
			}
		}(i)
	}
	wg.Wait()

	// Reload config.yaml and confirm every profile persisted to disk.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(cfg.AccessProfiles) != n {
		t.Errorf("config.yaml has %d profiles, want %d (concurrent writes lost some)", len(cfg.AccessProfiles), n)
	}
	for i := 0; i < n; i++ {
		id := "p" + strconv.Itoa(i)
		if _, ok := cfg.AccessProfiles[id]; !ok {
			t.Errorf("profile %s missing from config.yaml after concurrent create", id)
		}
		if !srv.router.HasAccessProfile(id) {
			t.Errorf("profile %s missing from live registry", id)
		}
	}
}

// TestCreateAccessProfile_OrphanSecretCleanedOnFailure locks the [PR#2360
// review MEDIUM] fix: when a token is written but a later step fails, the
// 0600 secret file must NOT be left orphaned on disk. We force the failure by
// supplying an unknown default_backend (rejected after the secret write).
func TestCreateAccessProfile_OrphanSecretCleanedOnFailure(t *testing.T) {
	srv, _, secretsDir := newCreateProfileServer(t)
	body := `{"id":"orphan","default_backend":"ghost","env":{"ANTHROPIC_BASE_URL":"https://api.anthropic.com"},"token_env_key":"ANTHROPIC_AUTH_TOKEN_FILE","token_content":"sk-should-be-cleaned"}`
	w := postCreateProfile(t, srv, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	secretPath := filepath.Join(secretsDir, "orphan.token")
	if _, err := os.Stat(secretPath); !os.IsNotExist(err) {
		t.Errorf("orphaned secret file was not cleaned up: stat err = %v", err)
	}
}
