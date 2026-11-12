package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/session"
)

// Edit then delete a profile through the settings page's endpoints: each
// lands in config.yaml and in the live registry, env untouched by the edit.
func TestAccessProfileEditAndDelete(t *testing.T) {
	srv, hs, cfgPath, _ := newCreateProfileServer(t)
	if w := postCreateProfile(t, hs, `{"id":"team","display_name":"Team","env":{"AWS_REGION":"us-west-2"}}`); w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/access-profiles", strings.NewReader(`{"id":"team","display_name":"Team B","aws_profile":"team-iso","default_model":"claude-opus-5-5[1m]"}`))
	w := httptest.NewRecorder()
	hs.accessProfilesH.HandleUpdate(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	live := srv.router.Backends().AccessProfiles()["team"]
	if live.DisplayName != "Team B" || live.AWSProfile != "team-iso" || live.Env["AWS_REGION"] != "us-west-2" {
		t.Errorf("live after update = %+v", live)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.AccessProfiles["team"]; got.DisplayName != "Team B" || got.Env["AWS_REGION"] != "us-west-2" {
		t.Errorf("config after update = %+v", got)
	}

	w = httptest.NewRecorder()
	hs.accessProfilesH.HandleDelete(w, httptest.NewRequest(http.MethodDelete, "/api/access-profiles?id=team", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if srv.router.Backends().HasAccessProfile("team") {
		t.Error("profile still live after delete")
	}
	if cfg, _ = config.Load(cfgPath); len(cfg.AccessProfiles) != 0 {
		t.Errorf("config after delete = %v", cfg.AccessProfiles)
	}
	w = httptest.NewRecorder()
	hs.accessProfilesH.HandleDelete(w, httptest.NewRequest(http.MethodDelete, "/api/access-profiles?id=team", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("second delete: %d, want 404", w.Code)
	}
}

// The model editor writes naozhi's settings file: add then remove, keeping
// the file's other keys and mode, with a backup per write.
func TestModelSettingsEndpoints(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "naozhi-settings.json")
	const doc = `{"model":"claude-opus-5-5[1m]","availableModels":["claude-opus-5-5[1m]"],"modelOverrides":{"claude-opus-5-5[1m]":"global.anthropic.claude-opus-5-5[1m]"},"awsCredentialExport":"keep-me"}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	_, hs := buildServerWithHandlers(ServerOptions{
		Addr: ":0", Router: session.NewRouter(session.RouterConfig{Workspace: dir}), Backend: "claude",
		Config: ConfigOptions{ModelSettingsPath: path},
	})
	h := hs.accessProfilesH

	w := httptest.NewRecorder()
	h.HandleAddModel(w, httptest.NewRequest(http.MethodPost, "/api/settings/models", strings.NewReader(`{"model":"global.anthropic.claude-fable-5-2[1m]"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	h.HandleModels(w, httptest.NewRequest(http.MethodGet, "/api/settings/models", nil))
	var list struct {
		Enabled bool `json:"enabled"`
		Models  []struct {
			ID, Profile string
		} `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || !list.Enabled || len(list.Models) != 2 {
		t.Fatalf("list = %s (%v)", w.Body, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"claude-fable-5-2[1m]": "global.anthropic.claude-fable-5-2[1m]"`) || !strings.Contains(string(data), "keep-me") {
		t.Errorf("settings after add:\n%s", data)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	if baks, _ := filepath.Glob(path + ".bak-*"); len(baks) == 0 {
		t.Error("no backup written")
	}

	w = httptest.NewRecorder()
	h.HandleRemoveModel(w, httptest.NewRequest(http.MethodDelete, "/api/settings/models?model=claude-opus-5-5%5B1m%5D", nil))
	if w.Code != http.StatusBadRequest {
		t.Errorf("removing the default model: %d, want 400", w.Code)
	}
	w = httptest.NewRecorder()
	h.HandleRemoveModel(w, httptest.NewRequest(http.MethodDelete, "/api/settings/models?model=global.anthropic.claude-fable-5-2%5B1m%5D", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", w.Code, w.Body)
	}
	if data, _ = os.ReadFile(path); strings.Contains(string(data), "fable") {
		t.Errorf("fable still in settings:\n%s", data)
	}
}
