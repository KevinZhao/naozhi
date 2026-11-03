package system

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/planner/workspace"
)

type fakeWorkspaces []workspace.Workspace

func (f fakeWorkspaces) List() []workspace.Workspace { return f }

func TestHandleHealth(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	snap := health.Snapshot{At: at, Unowned: 2, Units: []health.Unit{
		{ID: "a", State: health.Idle, Workspace: "ws_1", LastActive: at.Add(-time.Minute)},
		{ID: "b", State: health.Active, LastActive: at},
		{ID: "c", State: health.Failed, ErrClass: health.ErrAuth, Workspace: "ws_1"},
		{ID: "d", State: health.Idle, Workspace: "ws_2", LastActive: at},
		{ID: "e", State: health.Lost},
		{ID: "f", State: "later"},
		{ID: "g", State: health.Waiting},
	}}
	tests := []struct {
		name      string
		deps      Deps
		wantIDs   []string
		wantNames map[string]string
		wantAt    time.Time
		unowned   int
	}{
		{name: "no state machine", wantIDs: []string{}},
		{
			name: "abnormal first, then most recently active",
			deps: Deps{
				Health:     func() health.Snapshot { return snap },
				Workspaces: fakeWorkspaces{{ID: "ws_1", Name: "naozhi"}},
			},
			wantIDs:   []string{"c", "e", "g", "f", "b", "d", "a"},
			wantNames: map[string]string{"a": "naozhi", "c": "naozhi"},
			wantAt:    at,
			unowned:   2,
		},
		{
			name:    "no workspace registry",
			deps:    Deps{Health: func() health.Snapshot { return snap }},
			wantIDs: []string{"c", "e", "g", "f", "b", "d", "a"},
			wantAt:  at,
			unowned: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := httptest.NewRecorder()
			New(tt.deps).HandleHealth(w, httptest.NewRequest(http.MethodGet, "/api/system/health", nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%q", w.Code, w.Body.String())
			}
			var got struct {
				At      time.Time `json:"at"`
				Unowned int       `json:"unowned"`
				Units   []struct {
					ID            string `json:"id"`
					WorkspaceName string `json:"workspace_name"`
				} `json:"units"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v; body=%q", err, w.Body.String())
			}
			if got.Units == nil {
				t.Fatalf("units is null, want an array; body=%q", w.Body.String())
			}
			ids := []string{}
			for _, u := range got.Units {
				ids = append(ids, u.ID)
				if want := tt.wantNames[u.ID]; u.WorkspaceName != want {
					t.Errorf("unit %s workspace_name = %q, want %q", u.ID, u.WorkspaceName, want)
				}
			}
			if !slices.Equal(ids, tt.wantIDs) {
				t.Errorf("order = %v, want %v", ids, tt.wantIDs)
			}
			if !got.At.Equal(tt.wantAt) || got.Unowned != tt.unowned {
				t.Errorf("at, unowned = %v, %d; want %v, %d", got.At, got.Unowned, tt.wantAt, tt.unowned)
			}
		})
	}
}
