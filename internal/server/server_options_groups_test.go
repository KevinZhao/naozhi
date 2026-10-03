package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// Every grouped option reaches the consumer it names, and not a same-typed
// neighbour: each field gets a distinct value, so a swap shows.
func TestServerOptions_GroupsReachTheirConsumers(t *testing.T) {
	projects, err := project.NewManager(t.TempDir(), project.PlannerDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	s, hs := buildServerWithHandlers(ServerOptions{
		Addr:           ":0",
		ProjectManager: projects,
		Router:         session.NewRouter(session.RouterConfig{}),
		Backend:        "claude",
		Identity:       IdentityOptions{WorkspaceID: "ws-id", WorkspaceName: "ws-name", Version: "v9.8.7"},
		Watchdog:       WatchdogOptions{NoOutput: 3 * time.Minute, Total: 7 * time.Minute},
		Features:       FeatureOptions{PublicTmp: true},
		Queue:          QueueOptions{MaxDepth: 3, CollectDelay: 70 * time.Millisecond, Mode: "interrupt"},
	})

	// The Orchestrator's queue is built from hs.wiring.queue (buildWSStack).
	if want := (turn.QueueOptions{MaxDepth: 3, CollectDelay: 70 * time.Millisecond, Mode: turn.ModeInterrupt}); hs.wiring.queue != want {
		t.Errorf("turn queue options = %+v, want %+v", hs.wiring.queue, want)
	}
	if s.noOutputTimeout != 3*time.Minute || s.totalTimeout != 7*time.Minute {
		t.Errorf("server timeouts = %v / %v, want 3m / 7m", s.noOutputTimeout, s.totalTimeout)
	}
	h := hs.healthH
	if h.workspaceID != "ws-id" || h.workspaceName != "ws-name" || h.version != "v9.8.7" {
		t.Errorf("health identity = %q / %q / %q", h.workspaceID, h.workspaceName, h.version)
	}
	if h.noOutputTimeout != 3*time.Minute || h.totalTimeout != 7*time.Minute ||
		h.noOutputTimeoutStr != "3m0s" || h.totalTimeoutStr != "7m0s" {
		t.Errorf("health timeouts = %v %v %q %q", h.noOutputTimeout, h.totalTimeout, h.noOutputTimeoutStr, h.totalTimeoutStr)
	}

	rec := httptest.NewRecorder()
	s.sessionH.HandleList(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	var sessions struct {
		Stats struct {
			WorkspaceID   string `json:"workspace_id"`
			WorkspaceName string `json:"workspace_name"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sessions); err != nil {
		t.Fatal(err)
	}
	if sessions.Stats.WorkspaceID != "ws-id" || sessions.Stats.WorkspaceName != "ws-name" {
		t.Errorf("sessions stats identity = %+v", sessions.Stats)
	}

	rec = httptest.NewRecorder()
	hs.systemH.HandleUpdateStatus(rec, httptest.NewRequest(http.MethodGet, "/api/system/update", nil))
	var update struct {
		Current string `json:"current"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &update); err != nil {
		t.Fatal(err)
	}
	if update.Current != "v9.8.7" {
		t.Errorf("update status current = %q, want the build version", update.Current)
	}

	// With PublicTmp on, the pseudo-project is served rather than refused as
	// an invalid project name.
	rec = httptest.NewRecorder()
	body := bytes.NewBufferString(`{"project":"__public_tmp__","paths":["naozhi-s8b-absent"]}`)
	hs.projectH.HandleFilesExists(rec, httptest.NewRequest(http.MethodPost, "/api/projects/files/exists", body))
	if rec.Code != http.StatusOK {
		t.Errorf("public tmp files/exists = %d %s, want 200", rec.Code, rec.Body)
	}
}
