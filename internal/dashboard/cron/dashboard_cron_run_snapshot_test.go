package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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

// TestHandleRunSnapshot_SecretRefs_Sanitized pins [R202606-SEC-1]: each
// secret_ref name must pass through osutil.SanitizeForLog before serialising,
// matching the Prompt/Model/ImageVersion edge. A manifest hand-edited on disk
// can carry a control/bidi rune in a ref name that would render dangerously.
func TestHandleRunSnapshot_SecretRefs_Sanitized(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	sched := snapshotTestScheduler(t, filepath.Join(tmp, "cron_jobs.json"))

	jobID, runID := strings.Repeat("a", 16), strings.Repeat("b", 16)
	// U+202E RIGHT-TO-LEFT OVERRIDE embedded in a ref name.
	taintedRef := "github‮token"
	sched.WriteSandboxSnapshotForTest(jobID, runID, "p", "haiku", "phase2", []string{taintedRef})

	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/"+runID+"/snapshot?job_id="+jobID, nil)
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	h.HandleRunSnapshot(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		SecretRefs []string `json:"secret_refs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.SecretRefs) != 1 {
		t.Fatalf("secret_refs = %v, want 1 element", resp.SecretRefs)
	}
	if strings.ContainsRune(resp.SecretRefs[0], '‮') {
		t.Errorf("secret_ref contains bidi override (U+202E); SanitizeForLog not applied: %q", resp.SecretRefs[0])
	}
	if resp.SecretRefs[0] == "" {
		t.Errorf("secret_ref empty after sanitise; non-control content should remain")
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

// TestHandleRunSnapshot_CrossOwnershipRejected pins [R20260614-GO-004]:
// a manifest whose job_id field does not match the URL job_id must return
// available:false (consistent with HandleRunDetail and HandleRunTranscript).
// We simulate a tampered-on-disk manifest by writing the JSON directly to the
// runsnapshots tree with a mismatched job_id inside the file.
func TestHandleRunSnapshot_CrossOwnershipRejected(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	sched := snapshotTestScheduler(t, storePath)

	queryJobID := strings.Repeat("a", 16)
	runID := strings.Repeat("b", 16)
	// actualJobID is different from queryJobID — the manifest is "owned" by
	// a different job but placed under queryJobID's directory (tampered).
	actualJobID := strings.Repeat("c", 16)

	// Construct runsnapshots/<queryJobID>/<runID>.json with job_id=actualJobID.
	snapDir := filepath.Join(tmp, "runsnapshots", queryJobID)
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	man := cronpkg.SandboxRunSnapshot{
		RunID:   runID,
		JobID:   actualJobID, // mismatch — different from queryJobID
		Model:   "haiku",
		SchemaV: 1,
	}
	b, _ := json.Marshal(man)
	if err := os.WriteFile(filepath.Join(snapDir, runID+".json"), b, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet,
		"/api/cron/runs/"+runID+"/snapshot?job_id="+queryJobID, nil)
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	h.HandleRunSnapshot(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp struct {
		Available bool `json:"available"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Available {
		t.Errorf("cross-job snapshot must return available:false; got available:true")
	}
}

// TestHandleRunSnapshot_PromptHashSanitized pins [R20260614-GO-005]: the
// PromptHash field must pass through osutil.SanitizeForLog before going on
// the wire, to strip control/bidi runes from a hand-edited manifest.
func TestHandleRunSnapshot_PromptHashSanitized(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	sched := snapshotTestScheduler(t, storePath)

	jobID := strings.Repeat("a", 16)
	runID := strings.Repeat("b", 16)

	// Write a manifest with a bidi-injected PromptHash. A real hash is 64 hex
	// chars; here we embed a RIGHT-TO-LEFT OVERRIDE (U+202E) to test sanitisation.
	// The hash must still look like valid JSON but carry the injected rune.
	taintedHash := strings.Repeat("a", 31) + "‮" + strings.Repeat("f", 32)

	snapDir := filepath.Join(tmp, "runsnapshots", jobID)
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	man := cronpkg.SandboxRunSnapshot{
		RunID:      runID,
		JobID:      jobID,
		PromptHash: taintedHash,
		Model:      "haiku",
		SchemaV:    1,
	}
	b, _ := json.Marshal(man)
	if err := os.WriteFile(filepath.Join(snapDir, runID+".json"), b, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet,
		"/api/cron/runs/"+runID+"/snapshot?job_id="+jobID, nil)
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	h.HandleRunSnapshot(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Available  bool   `json:"available"`
		PromptHash string `json:"prompt_hash"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Available {
		t.Fatal("available must be true for a present manifest")
	}
	if strings.ContainsRune(resp.PromptHash, '‮') {
		t.Errorf("prompt_hash contains bidi override (U+202E); SanitizeForLog not applied: %q", resp.PromptHash)
	}
}
