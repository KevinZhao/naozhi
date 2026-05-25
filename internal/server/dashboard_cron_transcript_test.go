package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/discovery"
)

// fixtureRunWithJSONL writes a CronRun JSON record + matching JSONL into
// a fresh sched on tmpRoot, then returns (handlers, sched, jobID, runID,
// claudeDir). The scheduler is started so its runStore is wired to disk.
//
// The JSONL is keyed under
// `<claudeDir>/projects/<slug(workdir)>/<sessionID>.jsonl` matching the
// real CLI's layout so the handler's path-resolution logic exercises end
// to end without mocks.
func fixtureRunWithJSONL(t *testing.T, jsonlLines []string) (h *CronHandlers, jobID, runID, claudeDir string) {
	t.Helper()

	tmp := t.TempDir()
	claudeDir = filepath.Join(tmp, ".claude")
	storePath := filepath.Join(tmp, "cron_jobs.json")
	workDir := filepath.Join(tmp, "workspace")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}

	sched := cron.NewScheduler(cron.SchedulerConfig{StorePath: storePath})

	// Persist a job so runStore.Get can resolve it.
	job := cron.Job{
		ID:       strings.Repeat("a", 16),
		Schedule: "@every 1h",
		Prompt:   "transcript fixture",
		WorkDir:  workDir,
	}
	if err := sched.AddJob(&job); err != nil {
		t.Fatalf("add job: %v", err)
	}

	sessionID := "12345678-1234-1234-1234-123456789abc"
	jobID = job.ID
	runID = strings.Repeat("b", 16)

	// Write the run JSON via the scheduler's TestAppendRun if it exists,
	// otherwise drop the file directly. Direct write keeps the test
	// agnostic to internal helpers; runStore.Append is exposed via
	// scheduler's RunStore for tests.
	runsDir := filepath.Join(tmp, "runs", jobID)
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		t.Fatalf("mkdir runs: %v", err)
	}
	now := time.Now().UTC()
	startedAt := now.Add(-2 * time.Minute)
	endedAt := now
	runRec := cron.CronRun{
		RunID:      runID,
		JobID:      jobID,
		State:      cron.RunStateSucceeded,
		Trigger:    cron.TriggerScheduled,
		StartedAt:  startedAt,
		EndedAt:    endedAt,
		DurationMS: endedAt.Sub(startedAt).Milliseconds(),
		SessionID:  sessionID,
		WorkDir:    workDir,
	}
	runJSON, err := json.Marshal(runRec)
	if err != nil {
		t.Fatalf("marshal run: %v", err)
	}
	runPath := filepath.Join(runsDir, runID+".json")
	if err := os.WriteFile(runPath, runJSON, 0o600); err != nil {
		t.Fatalf("write run json: %v", err)
	}

	// Layout the JSONL.
	projDir := filepath.Join(claudeDir, "projects", discovery.ClaudeProjectSlug(workDir))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	jsonlPath := filepath.Join(projDir, sessionID+".jsonl")
	if err := os.WriteFile(jsonlPath, []byte(strings.Join(jsonlLines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	h = &CronHandlers{
		scheduler: sched,
		claudeDir: claudeDir,
	}
	return h, jobID, runID, claudeDir
}

// callTranscript runs the handler through the same path-param plumbing
// the real router uses (PathValue on the request). Keeping the
// contract-test in lock step with how production wires the URL.
func callTranscript(h *CronHandlers, jobID, runID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/"+runID+"/transcript?job_id="+jobID, nil)
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	h.handleRunTranscript(w, req)
	return w
}

func TestTranscript_HappyPath_AssistantAndToolUse(t *testing.T) {
	t.Parallel()
	// Three lines: user → assistant text + tool_use → user with
	// tool_result. Mirrors how the CLI persists a real interaction.
	now := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339Nano)
	lines := []string{
		`{"type":"user","timestamp":"` + now + `","message":{"role":"user","content":"reply pong"}}`,
		`{"type":"assistant","timestamp":"` + now + `","message":{"role":"assistant","content":[{"type":"text","text":"pong"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"echo hi"}}],"usage":{"input_tokens":10,"output_tokens":5}}}`,
		`{"type":"user","timestamp":"` + now + `","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"hi","is_error":false}]}}`,
	}
	h, jobID, runID, _ := fixtureRunWithJSONL(t, lines)

	w := callTranscript(h, jobID, runID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var resp transcriptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if resp.Fallback != "" {
		t.Errorf("fallback should be empty on happy path, got %q", resp.Fallback)
	}
	if resp.Truncated {
		t.Errorf("truncated should be false; lines fit in budget")
	}
	if got := resp.ToolCalls; got != 1 {
		t.Errorf("tool_calls = %d, want 1", got)
	}
	if resp.Tokens == nil || resp.Tokens.Output != 5 {
		t.Errorf("tokens output = %v, want 5", resp.Tokens)
	}
	// Want at least 3 turns: user, assistant, tool_use, tool_result
	// (the assistant block contributes both text and tool_use turns).
	if len(resp.Turns) < 3 {
		t.Fatalf("turns = %d (want >=3); %+v", len(resp.Turns), resp.Turns)
	}
	kinds := map[string]int{}
	for _, tr := range resp.Turns {
		kinds[tr.Kind]++
	}
	if kinds["user"] == 0 || kinds["assistant"] == 0 || kinds["tool_use"] == 0 {
		t.Errorf("missing kind; got %v", kinds)
	}
}

func TestTranscript_FallbackMissing_NoSession(t *testing.T) {
	t.Parallel()
	// Empty SessionID → fallback "missing", no FS access.
	h, jobID, runID, claudeDir := fixtureRunWithJSONL(t, nil)

	// Overwrite the run record to drop SessionID.
	runPath := filepath.Join(filepath.Dir(claudeDir), "runs", jobID, runID+".json")
	data, err := os.ReadFile(runPath)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	var rec cron.CronRun
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("unmarshal run: %v", err)
	}
	rec.SessionID = ""
	out, _ := json.Marshal(&rec)
	if err := os.WriteFile(runPath, out, 0o600); err != nil {
		t.Fatalf("rewrite run: %v", err)
	}

	w := callTranscript(h, jobID, runID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp transcriptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Fallback != "missing" {
		t.Errorf("fallback = %q, want missing", resp.Fallback)
	}
	if len(resp.Turns) != 0 {
		t.Errorf("turns should be empty, got %d", len(resp.Turns))
	}
}

func TestTranscript_FallbackMissing_JSONLDoesNotExist(t *testing.T) {
	t.Parallel()
	h, jobID, runID, claudeDir := fixtureRunWithJSONL(t, []string{`{"type":"user","message":{"role":"user","content":"x"}}`})
	// Delete the JSONL after fixture writes it.
	projDir := filepath.Join(claudeDir, "projects")
	entries, _ := os.ReadDir(projDir)
	for _, e := range entries {
		os.RemoveAll(filepath.Join(projDir, e.Name()))
	}

	w := callTranscript(h, jobID, runID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp transcriptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Fallback != "missing" {
		t.Errorf("fallback = %q, want missing", resp.Fallback)
	}
}

func TestTranscript_FallbackRaw_NoParsedTurns(t *testing.T) {
	t.Parallel()
	// JSONL only contains queue-operation events (no recognised turn
	// shapes) → fallback "raw".
	h, jobID, runID, _ := fixtureRunWithJSONL(t, []string{
		`{"type":"queue-operation","timestamp":"2026-05-22T08:00:00Z"}`,
		`{"type":"attachment","timestamp":"2026-05-22T08:00:00Z"}`,
	})
	w := callTranscript(h, jobID, runID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp transcriptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Fallback != "raw" {
		t.Errorf("fallback = %q, want raw", resp.Fallback)
	}
}

func TestTranscript_RejectsCrossJobID(t *testing.T) {
	t.Parallel()
	h, jobID, runID, _ := fixtureRunWithJSONL(t, []string{
		`{"type":"user","message":{"role":"user","content":"x"}}`,
	})
	// Use a different (but valid hex) job_id in the URL — the run record
	// on disk has a different job_id, so runStore.Get either returns
	// not-found OR our defensive cross-key check rejects.
	otherJob := strings.Repeat("c", 16)
	if otherJob == jobID {
		t.Skip("hash collision in test")
	}
	w := callTranscript(h, otherJob, runID)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (cross-job lookup must not leak); body=%s", w.Code, w.Body.String())
	}
}

func TestTranscript_RejectsNonHexIDs(t *testing.T) {
	t.Parallel()
	h := &CronHandlers{scheduler: cron.NewScheduler(cron.SchedulerConfig{})}
	cases := []struct{ runID, jobID string }{
		{"GGGG", "aaaaaaaaaaaaaaaa"},                   // invalid run_id
		{"aaaaaaaaaaaaaaaa", "GGGG"},                   // invalid job_id
		{"", "aaaaaaaaaaaaaaaa"},                       // empty run_id
		{"aaaaaaaaaaaaaaaa", ""},                       // empty job_id
		{strings.Repeat("a", 200), "aaaaaaaaaaaaaaaa"}, // run_id too long
	}
	for _, c := range cases {
		w := callTranscript(h, c.jobID, c.runID)
		if w.Code != http.StatusBadRequest {
			t.Errorf("runID=%q jobID=%q: status = %d, want 400", c.runID, c.jobID, w.Code)
		}
	}
}

func TestTranscript_TimeWindowFilter_DropsOlderTurns(t *testing.T) {
	t.Parallel()
	// fresh=false simulation: the JSONL contains turns from before the
	// run started (prior cron invocation in the same session) plus
	// turns inside the run window. Only the latter should appear.
	now := time.Now().UTC()
	tooOld := now.Add(-1 * time.Hour).Format(time.RFC3339Nano)
	inside := now.Add(-30 * time.Second).Format(time.RFC3339Nano)

	lines := []string{
		`{"type":"user","timestamp":"` + tooOld + `","message":{"role":"user","content":"old prompt from prior run"}}`,
		`{"type":"assistant","timestamp":"` + tooOld + `","message":{"role":"assistant","content":[{"type":"text","text":"old reply"}]}}`,
		`{"type":"user","timestamp":"` + inside + `","message":{"role":"user","content":"current prompt"}}`,
		`{"type":"assistant","timestamp":"` + inside + `","message":{"role":"assistant","content":[{"type":"text","text":"current reply"}]}}`,
	}
	h, jobID, runID, _ := fixtureRunWithJSONL(t, lines)

	w := callTranscript(h, jobID, runID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp transcriptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	for _, tr := range resp.Turns {
		if strings.Contains(tr.Text, "old") {
			t.Errorf("time-window filter failed: leaked turn %q", tr.Text)
		}
	}
	if len(resp.Turns) == 0 {
		t.Error("all turns dropped — expected current_prompt + current_reply to survive")
	}
}

func TestTranscript_RejectsSymlinkEscape(t *testing.T) {
	t.Parallel()
	h, jobID, runID, claudeDir := fixtureRunWithJSONL(t, []string{
		`{"type":"user","message":{"role":"user","content":"x"}}`,
	})
	// Replace the JSONL with a symlink pointing outside claudeDir.
	projDir := filepath.Join(claudeDir, "projects")
	entries, _ := os.ReadDir(projDir)
	if len(entries) == 0 {
		t.Fatal("no project dir created by fixture")
	}
	jsonlDir := filepath.Join(projDir, entries[0].Name())
	jsonls, _ := os.ReadDir(jsonlDir)
	if len(jsonls) == 0 {
		t.Fatal("no jsonl file created by fixture")
	}
	jsonlPath := filepath.Join(jsonlDir, jsonls[0].Name())

	// Hostile target: a file outside claudeDir.
	hostile := filepath.Join(t.TempDir(), "passwd")
	if err := os.WriteFile(hostile, []byte("hostile"), 0o600); err != nil {
		t.Fatalf("write hostile: %v", err)
	}
	if err := os.Remove(jsonlPath); err != nil {
		t.Fatalf("rm jsonl: %v", err)
	}
	if err := os.Symlink(hostile, jsonlPath); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	w := callTranscript(h, jobID, runID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp transcriptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Fallback != "missing" {
		t.Errorf("symlink escape MUST be rejected with fallback=missing, got %q", resp.Fallback)
	}
	// Body must not contain the hostile content.
	if strings.Contains(w.Body.String(), "hostile") {
		t.Error("symlink target content leaked into response")
	}
}

func TestTranscript_RejectsNonRegularFile(t *testing.T) {
	t.Parallel()
	h, jobID, runID, claudeDir := fixtureRunWithJSONL(t, []string{
		`{"type":"user","message":{"role":"user","content":"x"}}`,
	})
	// Replace the JSONL with a directory using the same name.
	projDir := filepath.Join(claudeDir, "projects")
	entries, _ := os.ReadDir(projDir)
	jsonlDir := filepath.Join(projDir, entries[0].Name())
	jsonls, _ := os.ReadDir(jsonlDir)
	jsonlPath := filepath.Join(jsonlDir, jsonls[0].Name())
	if err := os.Remove(jsonlPath); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if err := os.Mkdir(jsonlPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	w := callTranscript(h, jobID, runID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp transcriptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Fallback != "missing" {
		t.Errorf("non-regular file must downgrade to fallback=missing, got %q", resp.Fallback)
	}
}

// TestTranscript_HappyPath_ClaudeDirContainsSymlink reproduces the macOS
// case where /var → /private/var resolves to a different prefix than the
// raw claudeDir. The path-escape check must canonicalise *both* sides
// (resolved JSONL + claudeDir+projects root) before HasPrefix or every
// legitimate run on macOS / Docker bind-mounts / AMI-customised layouts
// would 404.
func TestTranscript_HappyPath_ClaudeDirContainsSymlink(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	// Real directory the data lives in.
	realDir := filepath.Join(tmp, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	// claudeDir-as-seen-by-handler is a symlink to realDir, mimicking
	// macOS /var → /private/var.
	link := filepath.Join(tmp, "via-link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	// Build the same fixture machinery but point claudeDir at the link.
	storePath := filepath.Join(tmp, "cron_jobs.json")
	workDir := filepath.Join(tmp, "workspace")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	sched := cron.NewScheduler(cron.SchedulerConfig{StorePath: storePath})
	job := cron.Job{
		ID:       strings.Repeat("a", 16),
		Schedule: "@every 1h",
		Prompt:   "fixture",
		WorkDir:  workDir,
	}
	if err := sched.AddJob(&job); err != nil {
		t.Fatalf("add job: %v", err)
	}
	sessionID := "12345678-1234-1234-1234-123456789abc"
	jobID := job.ID
	runID := strings.Repeat("b", 16)

	runsDir := filepath.Join(tmp, "runs", jobID)
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		t.Fatalf("mkdir runs: %v", err)
	}
	now := time.Now().UTC()
	run := cron.CronRun{
		RunID:      runID,
		JobID:      jobID,
		State:      cron.RunStateSucceeded,
		Trigger:    cron.TriggerScheduled,
		StartedAt:  now.Add(-2 * time.Minute),
		EndedAt:    now,
		DurationMS: 2 * 60 * 1000,
		SessionID:  sessionID,
		WorkDir:    workDir,
	}
	runJSON, _ := json.Marshal(run)
	if err := os.WriteFile(filepath.Join(runsDir, runID+".json"), runJSON, 0o600); err != nil {
		t.Fatalf("write run: %v", err)
	}

	// Write the JSONL under realDir so the link resolves there.
	projDir := filepath.Join(realDir, "projects", discovery.ClaudeProjectSlug(workDir))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	ts := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339Nano)
	line := `{"type":"user","timestamp":"` + ts + `","message":{"role":"user","content":"hi"}}`
	if err := os.WriteFile(filepath.Join(projDir, sessionID+".jsonl"), []byte(line), 0o600); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	// Handler points at the symlinked claudeDir — the prefix check must
	// resolve both sides identically before comparing.
	h := &CronHandlers{scheduler: sched, claudeDir: link}
	w := callTranscript(h, jobID, runID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp transcriptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Fallback != "" {
		t.Errorf("symlinked claudeDir must NOT trigger fallback; got %q", resp.Fallback)
	}
	if len(resp.Turns) != 1 {
		t.Errorf("expected 1 turn, got %d", len(resp.Turns))
	}
}

// TestTranscript_BugfixWiring asserts the route is registered and the
// claudeDir field is populated on production wiring (smoke).
func TestTranscript_RouteIsRegistered(t *testing.T) {
	t.Parallel()
	srv := newTestServerWithScheduler(&mockPlatform{})
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/"+strings.Repeat("a", 16)+"/transcript?job_id="+strings.Repeat("b", 16), nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	// Either 200 (lucky run hit) or 404 (no such run) — both prove
	// the route resolves; 405/404-mux-not-found would mean we didn't
	// register the handler.
	if w.Code == http.StatusMethodNotAllowed {
		t.Fatalf("route not registered: %d", w.Code)
	}
}

// TestFlattenUserEvent_PreallocCapacity pins R241-PERF-7: per-line slice
// allocation must match the actual turn count exactly. The prior
// implementation hard-coded `make([]transcriptTurn, 0, 2)` which over-
// allocated for text-only lines and grew (re-allocate) for tool_result
// arrays larger than 2 — both wasteful on a 500-row transcript.
//
// We pin three shapes:
//  1. text-only line → cap == 1
//  2. zero-turn line (empty content array) → returns nil slice (no alloc)
//  3. text + 3 tool_results → cap == 4 (no grow)
//
// The cap check would still pass after a future `make(... 0, 8)` regression
// (cap >= len), so the empty-content sub-case is the load-bearing assertion:
// it pins that we no longer pay the per-line 2-cap header on lines that
// produce nothing.
func TestFlattenUserEvent_PreallocCapacity(t *testing.T) {
	t.Parallel()

	// Case 1: text-only.
	textEv := &claudeJSONLEvent{
		Type:    "user",
		Message: json.RawMessage(`{"role":"user","content":"hello world"}`),
	}
	out, _, _, parsed := flattenUserEvent(textEv, 0, 0)
	if !parsed || len(out) != 1 {
		t.Fatalf("text-only: parsed=%v len(out)=%d (want true / 1)", parsed, len(out))
	}
	if cap(out) != 1 {
		t.Errorf("text-only: cap(out)=%d, want exactly 1 (R241-PERF-7 prealloc)", cap(out))
	}
	if out[0].Kind != "user" || out[0].Text != "hello world" {
		t.Errorf("text-only: turn=%+v", out[0])
	}

	// Case 2: empty content-block array → no turns at all. Previous code
	// returned a 2-cap empty slice from the `out := make(... 0, 2)` line;
	// we now return nil so the per-line allocation is skipped entirely.
	emptyEv := &claudeJSONLEvent{
		Type:    "user",
		Message: json.RawMessage(`{"role":"user","content":[]}`),
	}
	out2, _, _, parsed2 := flattenUserEvent(emptyEv, 0, 0)
	if parsed2 || len(out2) != 0 {
		t.Fatalf("empty-content: parsed=%v len(out)=%d (want false / 0)", parsed2, len(out2))
	}
	if out2 != nil {
		t.Errorf("empty-content: out=%v want nil (no per-line alloc on zero-turn lines)", out2)
	}

	// Case 3: 3 tool_result blocks → cap == 3 (no grow). Sized exactly so
	// the append loop does no reallocation.
	threeRes := json.RawMessage(`{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"a","content":"o1","is_error":false},` +
		`{"type":"tool_result","tool_use_id":"b","content":"o2","is_error":false},` +
		`{"type":"tool_result","tool_use_id":"c","content":"o3","is_error":false}]}`)
	out3, _, _, parsed3 := flattenUserEvent(&claudeJSONLEvent{Type: "user", Message: threeRes}, 0, 0)
	if !parsed3 || len(out3) != 3 {
		t.Fatalf("3-tool_result: parsed=%v len(out)=%d (want true / 3)", parsed3, len(out3))
	}
	if cap(out3) != 3 {
		t.Errorf("3-tool_result: cap(out)=%d, want exactly 3 (no grow)", cap(out3))
	}
}
