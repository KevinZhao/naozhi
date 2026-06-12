package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

func snapshotTestScheduler(t *testing.T, storePath string) *cronpkg.Scheduler {
	t.Helper()
	return cronpkg.NewScheduler(cronpkg.SchedulerConfig{
		StorePath:      storePath,
		AllowNilRouter: true,
	}, cronpkg.SchedulerDeps{})
}

// TestHandleRunSnapshot_ServesManifest pins the §7.3 snapshot endpoint:
// a written snapshot is returned with available:true + manifest + prompt.
func TestHandleRunSnapshot_ServesManifest(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	sched := snapshotTestScheduler(t, storePath)

	jobID, runID := strings.Repeat("a", 16), strings.Repeat("b", 16)
	sched.WriteSandboxSnapshotForTest(jobID, runID, "the cloud prompt", "haiku", "phase2", []string{"github_token"})

	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/"+runID+"/snapshot?job_id="+jobID, nil)
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	h.HandleRunSnapshot(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Available    bool     `json:"available"`
		Prompt       string   `json:"prompt"`
		Model        string   `json:"model"`
		ImageVersion string   `json:"image_version"`
		SecretRefs   []string `json:"secret_refs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Available {
		t.Fatal("available must be true for a written snapshot")
	}
	if resp.Prompt != "the cloud prompt" || resp.Model != "haiku" || resp.ImageVersion != "phase2" {
		t.Fatalf("manifest fields wrong: %+v", resp)
	}
	if len(resp.SecretRefs) != 1 || resp.SecretRefs[0] != "github_token" {
		t.Fatalf("secret refs = %v, want [github_token]", resp.SecretRefs)
	}
}

// TestHandleRunSnapshot_MissingUnavailable: a run with no snapshot returns
// 200 + available:false (not 404), so the panel renders a deterministic
// "unavailable" state.
func TestHandleRunSnapshot_MissingUnavailable(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	sched := snapshotTestScheduler(t, filepath.Join(tmp, "cron_jobs.json"))

	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet,
		"/api/cron/runs/"+strings.Repeat("b", 16)+"/snapshot?job_id="+strings.Repeat("a", 16), nil)
	req.SetPathValue("run_id", strings.Repeat("b", 16))
	w := httptest.NewRecorder()
	h.HandleRunSnapshot(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"available":false`) {
		t.Fatalf("want available:false, got %s", w.Body.String())
	}
}

// TestHandleRunSnapshot_RejectsBadIDs guards the path-traversal surface.
func TestHandleRunSnapshot_RejectsBadIDs(t *testing.T) {
	t.Parallel()
	sched := snapshotTestScheduler(t, filepath.Join(t.TempDir(), "cron_jobs.json"))
	h := &Handlers{scheduler: sched}

	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/x/snapshot?job_id="+strings.Repeat("a", 16), nil)
	req.SetPathValue("run_id", "../../etc")
	w := httptest.NewRecorder()
	h.HandleRunSnapshot(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad run_id status = %d, want 400", w.Code)
	}
}
