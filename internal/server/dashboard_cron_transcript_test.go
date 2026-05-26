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

// TestTranscript_FreshFalse_DropsTimestampLessEvents pins R240-SEC-15 /
// #1046: in fresh=false mode (shared JSONL across cron runs) any event
// without an explicit timestamp must NOT be returned, because the
// time-window gate cannot attribute it to a specific run and an
// adjacent-run "queue-operation" / untimestamped attachment would
// otherwise leak into this run's transcript.
func TestTranscript_FreshFalse_DropsTimestampLessEvents(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	inside := now.Add(-30 * time.Second).Format(time.RFC3339Nano)

	// Mix of dated (in-window) + un-dated events. Un-dated events must
	// be dropped because run.Fresh defaults to false in the fixture.
	lines := []string{
		`{"type":"user","timestamp":"` + inside + `","message":{"role":"user","content":"in-window prompt"}}`,
		`{"type":"assistant","timestamp":"` + inside + `","message":{"role":"assistant","content":[{"type":"text","text":"in-window reply"}]}}`,
		// Un-dated user event with content that would betray a leak if
		// it accidentally surfaced. In fresh=false mode this is
		// indistinguishable from "from an adjacent run".
		`{"type":"user","message":{"role":"user","content":"LEAKED_FROM_ADJACENT_RUN"}}`,
		// Un-dated assistant event likewise.
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"LEAKED_REPLY"}]}}`,
	}
	h, jobID, runID, _ := fixtureRunWithJSONL(t, lines)

	w := callTranscript(h, jobID, runID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp transcriptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	for _, tr := range resp.Turns {
		if strings.Contains(tr.Text, "LEAK") {
			t.Errorf("fresh=false leaked timestamp-less turn %q (R240-SEC-15)", tr.Text)
		}
	}
	// In-window turns must still appear.
	if len(resp.Turns) == 0 {
		t.Error("all turns dropped — expected the in-window pair to survive")
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

// TestSameFileAncestor exercises the path-containment helper that backs the
// case-insensitive fallback for the path-escape gate (R238-SEC-6). The
// helper must:
//   - return true when root == resolved (same inode trivially).
//   - return true when resolved is N levels under root.
//   - return false when resolved escapes the root subtree.
//   - return false when root does not exist (Stat error).
//   - traverse symlinks at the call sites — but the helper itself takes the
//     already-resolved paths, so symlink behaviour is covered by the
//     handler-level happy-path test above.
func TestSameFileAncestor(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "claude", "projects")
	deep := filepath.Join(root, "slug", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(deep), 0o755); err != nil {
		t.Fatalf("mkdir deep: %v", err)
	}
	if err := os.WriteFile(deep, []byte("x"), 0o600); err != nil {
		t.Fatalf("write deep: %v", err)
	}
	outside := filepath.Join(tmp, "elsewhere", "x.jsonl")
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}

	if !sameFileAncestor(root, root) {
		t.Errorf("root == resolved must be contained")
	}
	if !sameFileAncestor(deep, root) {
		t.Errorf("deep child must be contained under root")
	}
	if sameFileAncestor(outside, root) {
		t.Errorf("path outside root must not be contained")
	}
	if sameFileAncestor(deep, filepath.Join(tmp, "does-not-exist")) {
		t.Errorf("missing root must return false rather than panic")
	}
}

// TestTranscript_TruncateReason_LineTooLong covers R240-SEC-8 / #1049: a
// JSONL line that exceeds maxTranscriptLineBytes must surface
// truncate_reason="line_too_long", distinct from a generic IO error or a
// size-cap hit. Forensics rely on this discrimination.
func TestTranscript_TruncateReason_LineTooLong(t *testing.T) {
	t.Parallel()
	// Build a single assistant line whose JSON-encoded size > maxTranscriptLineBytes.
	// We pad the assistant's text content with a single huge string so the
	// resulting JSONL line is well over the per-line cap.
	now := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339Nano)
	pad := strings.Repeat("x", maxTranscriptLineBytes+8*1024)
	bigLine := `{"type":"assistant","timestamp":"` + now + `","message":{"role":"assistant","content":[{"type":"text","text":"` + pad + `"}],"usage":{"input_tokens":1,"output_tokens":1}}}`
	h, jobID, runID, _ := fixtureRunWithJSONL(t, []string{bigLine})

	w := callTranscript(h, jobID, runID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp transcriptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if !resp.Truncated {
		t.Fatalf("truncated should be true (line exceeded maxTranscriptLineBytes)")
	}
	if resp.TruncateReason != "line_too_long" {
		t.Errorf("truncate_reason = %q, want %q", resp.TruncateReason, "line_too_long")
	}
}

// TestTranscript_TruncateReason_SizeCap covers the size_cap branch of
// R240-SEC-8 / #1049: hitting maxTranscriptTurns must surface
// truncate_reason="size_cap", not "line_too_long" or "scan_io_error".
func TestTranscript_TruncateReason_SizeCap(t *testing.T) {
	t.Parallel()
	// maxTranscriptTurns=500 ; emit 510 user turns, all within the time
	// window. The handler should stop at the cap and report size_cap.
	now := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339Nano)
	lines := make([]string, 0, maxTranscriptTurns+10)
	for i := 0; i < maxTranscriptTurns+10; i++ {
		lines = append(lines, `{"type":"user","timestamp":"`+now+`","message":{"role":"user","content":"hi"}}`)
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
	if !resp.Truncated {
		t.Fatalf("truncated should be true (turns >= cap)")
	}
	if resp.TruncateReason != "size_cap" {
		t.Errorf("truncate_reason = %q, want %q", resp.TruncateReason, "size_cap")
	}
}

// TestFlattenAssistantEvent_ToolInputSizeCap pins R234-SEC-8: tool_use.Input
// JSON exceeding maxToolInputBytes must be replaced with the [truncated]
// placeholder. Without this guard a transcript with 500 turns × 256KB
// tool_use.Input lines would amplify the response by ~128MB. We assert
// both the small-input pass-through (Input bytes returned verbatim) and
// the over-cap replacement, plus that summary still survives the cap so
// the timeline label is preserved.
func TestFlattenAssistantEvent_ToolInputSizeCap(t *testing.T) {
	t.Parallel()

	// Small Input — passes through unchanged.
	smallInput := `{"command":"echo hi"}`
	smallEv := &claudeJSONLEvent{
		Type: "assistant",
		Message: json.RawMessage(`{"role":"assistant","content":[` +
			`{"type":"tool_use","id":"tu_a","name":"Bash","input":` + smallInput + `}` +
			`]}`),
	}
	out, _, _, parsed := flattenAssistantEvent(smallEv, 0, 0)
	if !parsed || len(out) != 1 {
		t.Fatalf("small input: parsed=%v len(out)=%d (want true / 1)", parsed, len(out))
	}
	if string(out[0].Input) != smallInput {
		t.Errorf("small input: Input=%q, want pass-through %q", string(out[0].Input), smallInput)
	}
	if !strings.Contains(out[0].Summary, "echo hi") {
		t.Errorf("small input: summary=%q lost label", out[0].Summary)
	}

	// Oversized Input — replaced with [truncated] placeholder. Pad the
	// command field to push raw Input bytes past maxToolInputBytes.
	pad := strings.Repeat("x", maxToolInputBytes+8*1024)
	bigInput := `{"command":"` + pad + `"}`
	bigEv := &claudeJSONLEvent{
		Type: "assistant",
		Message: json.RawMessage(`{"role":"assistant","content":[` +
			`{"type":"tool_use","id":"tu_b","name":"Bash","input":` + bigInput + `}` +
			`]}`),
	}
	out2, _, _, parsed2 := flattenAssistantEvent(bigEv, 0, 0)
	if !parsed2 || len(out2) != 1 {
		t.Fatalf("big input: parsed=%v len(out)=%d (want true / 1)", parsed2, len(out2))
	}
	if string(out2[0].Input) != `"[truncated]"` {
		t.Errorf("big input: Input=%q, want %q (R234-SEC-8 cap)", string(out2[0].Input), `"[truncated]"`)
	}
	if len(out2[0].Input) > maxToolInputBytes {
		t.Errorf("big input: Input bytes=%d, must be <= maxToolInputBytes=%d after truncation", len(out2[0].Input), maxToolInputBytes)
	}
	// Summary derives from a probe-Unmarshal of the original Input bytes
	// before truncation (capped to 200 chars by SanitizeForLog), so the
	// timeline label still surfaces even though raw Input was dropped.
	if out2[0].Summary == "" {
		t.Errorf("big input: summary empty; expected probe-derived label to survive cap")
	}
}

// TestSummariseToolInput_FallbackUsesRawBytes pins R244-GO-P2-2 (#909):
// when summariseToolInput's typed-probe finds no recognised key, the
// fallback path must hand the ORIGINAL raw input bytes to SanitizeForLog
// rather than re-Marshalling the probe struct. A re-Marshal would (a)
// allocate a fresh buffer per call AND (b) silently reorder keys
// alphabetically because encoding/json sorts struct fields by declaration
// order — both observable regressions if a future refactor swaps the
// fallback back to json.Marshal(obj). Asserting `zeta` precedes `alpha`
// in the surfaced summary catches the alphabetical-reorder symptom; an
// empty result on broken JSON catches the early-return path; recognised
// fields still winning over fallback locks priority semantics.
func TestSummariseToolInput_FallbackUsesRawBytes(t *testing.T) {
	t.Parallel()

	// 1. Fallback branch (no recognised key): summary must preserve the
	//    original key order (zeta-then-alpha). A re-Marshal would emit
	//    keys in struct-declaration order (alphabetical for an unknown
	//    key map fallback) and lose this property.
	raw := json.RawMessage(`{"zeta":1,"alpha":2}`)
	got := summariseToolInput("CustomTool", raw)
	zetaAt := strings.Index(got, "zeta")
	alphaAt := strings.Index(got, "alpha")
	if zetaAt < 0 || alphaAt < 0 {
		t.Fatalf("fallback: summary=%q lost both keys", got)
	}
	if zetaAt > alphaAt {
		t.Errorf("fallback: zeta should precede alpha (got %q); a re-Marshal regression would reorder", got)
	}

	// 2. Broken JSON returns empty (early return on Unmarshal error).
	if got := summariseToolInput("X", json.RawMessage(`{not-json`)); got != "" {
		t.Errorf("broken JSON: summary=%q, want empty", got)
	}

	// 3. Recognised priority field (Command) still wins over fallback —
	//    even when other fallback-eligible keys are present, the typed
	//    probe path must short-circuit before reaching the raw-bytes
	//    fallback.
	raw3 := json.RawMessage(`{"command":"ls -la","other":"ignored"}`)
	got3 := summariseToolInput("Bash", raw3)
	if !strings.Contains(got3, "ls -la") {
		t.Errorf("priority: summary=%q, want to contain command label", got3)
	}
	if strings.Contains(got3, "ignored") {
		t.Errorf("priority: summary=%q leaked fallback raw bytes despite recognised key", got3)
	}

	// 4. Empty input returns empty (zero-byte short-circuit).
	if got := summariseToolInput("X", json.RawMessage(``)); got != "" {
		t.Errorf("empty input: summary=%q, want empty", got)
	}
}

// TestSummariseToolInput_OversizeBypassesUnmarshal pins R242-SEC-13 (#645):
// inputs that exceed summariseToolInputMaxBytes must skip json.Unmarshal
// entirely and go directly to the byte-truncated fallback. This bounds
// the worst-case decode cost regardless of how deeply nested or pathological
// the input JSON is — even if the upstream maxTranscriptLineBytes (256 KB)
// bounded the raw line, a single ridiculously-nested tool_use Input could
// otherwise still pay the full Unmarshal stack-walk cost just to produce
// a 200-char label that we'd ultimately truncate anyway.
//
// We verify two contracts:
//
//   - Output is non-empty for an oversize input that is otherwise
//     well-formed JSON: the fallback path through SanitizeForLog must
//     surface the prefix.
//   - Even for an OVERSIZE input that is malformed JSON (would normally
//     cause json.Unmarshal to return an error and summariseToolInput to
//     return ""), the oversize bypass produces a non-empty truncated
//     prefix — proving the Unmarshal step is never invoked for oversize
//     inputs.
func TestSummariseToolInput_OversizeBypassesUnmarshal(t *testing.T) {
	t.Parallel()

	// Build an oversize but otherwise well-formed JSON object with a
	// recognised priority field. The well-formed path wouldn't take the
	// fallback branch — but at the oversize threshold the bypass forces
	// the raw-bytes fallback regardless. So the output must be a
	// truncated prefix of the raw JSON, NOT the recognised "command"
	// value the probe would have lifted.
	pad := strings.Repeat("x", summariseToolInputMaxBytes+1)
	raw := json.RawMessage(`{"command":"ls","filler":"` + pad + `"}`)
	if len(raw) <= summariseToolInputMaxBytes {
		t.Fatalf("test setup: raw len %d not > cap %d", len(raw), summariseToolInputMaxBytes)
	}
	got := summariseToolInput("Bash", raw)
	if got == "" {
		t.Errorf("oversize well-formed: summary unexpectedly empty (oversize bypass should yield truncated raw)")
	}
	// SanitizeForLog caps at 200 chars, so the bypass output must not
	// exceed that ceiling regardless of input size.
	if len(got) > 256 {
		t.Errorf("oversize: summary len=%d exceeds 256 byte SanitizeForLog ceiling (200 chars + UTF-8 slack)", len(got))
	}

	// Oversize MALFORMED input: if the bypass were missing, json.Unmarshal
	// would fail and summariseToolInput would return "". With the bypass,
	// the raw-bytes fallback runs and produces a non-empty prefix — the
	// canary proving the bypass is taking the Unmarshal off the hot path
	// even on hostile inputs.
	rawBroken := json.RawMessage(`{not-valid-json-` + pad)
	if len(rawBroken) <= summariseToolInputMaxBytes {
		t.Fatalf("test setup: rawBroken len %d not > cap %d", len(rawBroken), summariseToolInputMaxBytes)
	}
	gotBroken := summariseToolInput("X", rawBroken)
	if gotBroken == "" {
		t.Errorf("oversize malformed: summary empty — bypass missing, Unmarshal-failure path swallowed the input")
	}
}

// TestFlattenJSONLEvent_DispatchByType pins R242-CR-13 (#704): the
// monolithic flattenJSONLEvent body was split into per-type helpers
// (flattenUserEvent / flattenAssistantEvent / flattenSystemEvent) wired
// through a one-line switch. Test the dispatch contract directly so a
// future refactor flattening the helpers back into one function (or
// dropping a case branch) trips before it ships:
//   - "user" routes to flattenUserEvent (text turn surfaces)
//   - "assistant" routes to flattenAssistantEvent (assistant turn surfaces)
//   - "system" routes to flattenSystemEvent (only error subtype emits)
//   - unknown event types return parsed=false (default branch holds)
func TestFlattenJSONLEvent_DispatchByType(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		ev        *claudeJSONLEvent
		wantKinds []string // ordered kinds expected in output
		wantParse bool
	}{
		{
			name: "user_text",
			ev: &claudeJSONLEvent{
				Type:    "user",
				Message: json.RawMessage(`{"role":"user","content":"hello"}`),
			},
			wantKinds: []string{"user"},
			wantParse: true,
		},
		{
			name: "assistant_text",
			ev: &claudeJSONLEvent{
				Type:    "assistant",
				Message: json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"hi"}]}`),
			},
			wantKinds: []string{"assistant"},
			wantParse: true,
		},
		{
			name: "system_error",
			ev: &claudeJSONLEvent{
				Type:    "system",
				Message: json.RawMessage(`{"subtype":"error","message":"boom"}`),
			},
			wantKinds: []string{"error"},
			wantParse: true,
		},
		{
			name: "system_init_skipped",
			ev: &claudeJSONLEvent{
				Type:    "system",
				Message: json.RawMessage(`{"subtype":"init"}`),
			},
			wantKinds: nil,
			wantParse: false,
		},
		{
			name:      "unknown_type_default",
			ev:        &claudeJSONLEvent{Type: "queue-operation", Message: json.RawMessage(`{}`)},
			wantKinds: nil,
			wantParse: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, _, _, parsed := flattenJSONLEvent(tc.ev, 0, 0)
			if parsed != tc.wantParse {
				t.Errorf("parsed=%v, want %v", parsed, tc.wantParse)
			}
			if len(out) != len(tc.wantKinds) {
				t.Fatalf("len(out)=%d, want %d (kinds=%v)", len(out), len(tc.wantKinds), tc.wantKinds)
			}
			for i, want := range tc.wantKinds {
				if out[i].Kind != want {
					t.Errorf("out[%d].Kind=%q, want %q", i, out[i].Kind, want)
				}
			}
		})
	}
}

// BenchmarkSummariseToolInput_TypedProbe locks the R233-PERF-5 / #695
// perf win: the typed `toolInputProbe` decode replaced the prior
// `map[string]any` decode + key hunt, halving allocations on the hot
// transcript-flatten path. A regression to map[string]any (e.g. someone
// "simplifying" the struct away) would visibly inflate allocs/op against
// the recorded baseline. Benchmarks the three input shapes the production
// code sees: priority-key short-circuit (Bash), priority-key fallthrough
// to FilePath, and the typed-probe-empty fallback to raw bytes.
func BenchmarkSummariseToolInput_TypedProbe(b *testing.B) {
	bashInput := json.RawMessage(`{"command":"echo hello","other":"x"}`)
	readInput := json.RawMessage(`{"file_path":"/tmp/x.txt"}`)
	customInput := json.RawMessage(`{"alpha":1,"beta":"long-fallback-text"}`)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = summariseToolInput("Bash", bashInput)
		_ = summariseToolInput("Read", readInput)
		_ = summariseToolInput("Custom", customInput)
	}
}

// TestMaxTranscriptBytes_Int64Type pins R244-GO-P2-3 (#911): the
// transcript reader directly constructs `&io.LimitedReader{N: …}`
// rather than calling io.LimitReader, so the source operand must be
// int64-typed for the LimitedReader.N field assignment to round-trip
// without truncation on any GOARCH. The current code does
// `N: int64(maxTranscriptBytes)` (explicit cast even though the const
// is already int64-typed) as defence-in-depth against a future cap
// change to `1 << 32` written without a `int64` suffix on a 32-bit
// platform — which would silently wrap maxTranscriptBytes to -2**31
// and turn the LimitedReader into an immediate-EOF reader, hiding
// transcript truncation. This test fails fast if the const ever loses
// its int64 type or the cap becomes representable only as int64.
func TestMaxTranscriptBytes_Int64Type(t *testing.T) {
	t.Parallel()
	// Compile-time-ish guard: assigning maxTranscriptBytes to an int64
	// must be lossless. If a future refactor makes the constant
	// int-typed, this still compiles on 64-bit but the explicit
	// int64() cast at the LimitedReader call site stays load-bearing
	// for 32-bit builds — assert the runtime value matches the
	// declared 8 MiB cap so a typo in the literal also fails the test.
	var n int64 = maxTranscriptBytes
	if want := int64(8 * 1024 * 1024); n != want {
		t.Errorf("maxTranscriptBytes = %d, want %d (8 MiB); literal drift breaks LimitedReader cap semantics", n, want)
	}
	if n <= 0 {
		t.Fatalf("maxTranscriptBytes = %d must be > 0; a non-positive value would make io.LimitedReader return EOF immediately and silently truncate every transcript", n)
	}
}

// TestTruncatedToolInputPlaceholder_ValidJSON pins R234-SEC-8 (#1018):
// the placeholder substituted for over-cap tool_use.Input must itself
// be valid JSON so the wire shape stays a json.RawMessage the dashboard
// can render with its existing JSON renderer. A previous fix introduced
// the cap and chose `"[truncated]"` as the placeholder; this test guards
// against a future refactor that swaps to a non-JSON sentinel like
// `[truncated]` (no quotes) or `null` (loses the truncation signal),
// either of which would break dashboard JSON parsing or hide the cap-hit
// from operators investigating an oversized run.
//
// We assert:
//  1. the placeholder Unmarshals as JSON without error (wire-shape safety)
//  2. it decodes to the literal string "[truncated]" (forensic signal)
//  3. its raw bytes do NOT exceed maxToolInputBytes (cap self-consistency
//     — replacing with a value that itself exceeds the cap would be a
//     logic regression)
func TestTruncatedToolInputPlaceholder_ValidJSON(t *testing.T) {
	t.Parallel()
	var s string
	if err := json.Unmarshal(truncatedToolInputPlaceholder, &s); err != nil {
		t.Fatalf("placeholder %q is not valid JSON: %v; dashboard JSON renderer would fail", string(truncatedToolInputPlaceholder), err)
	}
	if s != "[truncated]" {
		t.Errorf("placeholder decoded to %q, want %q (forensic signal for cap-hit operators)", s, "[truncated]")
	}
	if len(truncatedToolInputPlaceholder) > maxToolInputBytes {
		t.Errorf("placeholder len=%d exceeds maxToolInputBytes=%d; replacement value must satisfy the cap it enforces", len(truncatedToolInputPlaceholder), maxToolInputBytes)
	}
}

// TestFlattenAssistantEvent_ToolInputSizeCap_MultiBlock pins R234-SEC-8
// (#1018) at the per-block-aggregation boundary: a single assistant
// message with N tool_use blocks must apply the cap independently to
// each block. Without this, an attacker could split the 256 KiB
// per-line budget across 4 × 65 KiB tool_use blocks and bypass the
// 64 KiB per-input cap by sneaking each one past the per-line gate.
// The flattenJSONLEvent loop must visit every block and rewrite each
// over-cap Input independently. Existing TestFlattenAssistantEvent_
// ToolInputSizeCap covers the single-block case; this test guards the
// loop-level invariant.
func TestFlattenAssistantEvent_ToolInputSizeCap_MultiBlock(t *testing.T) {
	t.Parallel()

	smallInput := `{"command":"echo small"}`
	pad := strings.Repeat("y", maxToolInputBytes+4*1024)
	bigInput := `{"command":"` + pad + `"}`

	// Mix small + big + small + big in one assistant event so the loop
	// must independently classify each block.
	ev := &claudeJSONLEvent{
		Type: "assistant",
		Message: json.RawMessage(`{"role":"assistant","content":[` +
			`{"type":"tool_use","id":"tu_1","name":"Bash","input":` + smallInput + `},` +
			`{"type":"tool_use","id":"tu_2","name":"Bash","input":` + bigInput + `},` +
			`{"type":"tool_use","id":"tu_3","name":"Bash","input":` + smallInput + `},` +
			`{"type":"tool_use","id":"tu_4","name":"Bash","input":` + bigInput + `}` +
			`]}`),
	}
	out, _, toolCalls, parsed := flattenAssistantEvent(ev, 0, 0)
	if !parsed || len(out) != 4 {
		t.Fatalf("multi-block: parsed=%v len(out)=%d (want true / 4)", parsed, len(out))
	}
	if toolCalls != 4 {
		t.Errorf("multi-block: toolCalls=%d, want 4 (every block counts toward the running total even after truncation)", toolCalls)
	}

	// Block 0 (small) — verbatim pass-through.
	if string(out[0].Input) != smallInput {
		t.Errorf("block 0 (small): Input=%q, want pass-through %q", string(out[0].Input), smallInput)
	}
	// Block 1 (big) — replaced.
	if string(out[1].Input) != `"[truncated]"` {
		t.Errorf("block 1 (big): Input=%q, want %q (must be capped independently)", string(out[1].Input), `"[truncated]"`)
	}
	// Block 2 (small) — verbatim, NOT collateral-damage from block 1's cap.
	if string(out[2].Input) != smallInput {
		t.Errorf("block 2 (small after big): Input=%q, want pass-through %q (cap must not bleed across blocks)", string(out[2].Input), smallInput)
	}
	// Block 3 (big) — replaced.
	if string(out[3].Input) != `"[truncated]"` {
		t.Errorf("block 3 (big): Input=%q, want %q", string(out[3].Input), `"[truncated]"`)
	}

	// Aggregate-bytes invariant: total Input bytes surfaced must stay
	// well below the per-line cap. With 2 small (~25 B each) + 2
	// placeholder (13 B each), total is ~76 B — vs. ~140 KiB if the
	// cap silently dropped. The 4 × maxToolInputBytes guard documents
	// the worst-case headroom an unbounded variant would consume.
	totalInputBytes := 0
	for _, t := range out {
		totalInputBytes += len(t.Input)
	}
	if totalInputBytes >= 4*maxToolInputBytes {
		t.Errorf("multi-block: total surfaced Input bytes=%d, must be << 4×maxToolInputBytes=%d (#1018 cap is per-block, not per-line)", totalInputBytes, 4*maxToolInputBytes)
	}
}

// TestFlattenAssistantEvent_AssistantFirstThenSequentialIndices pins
// R247-PERF-2 / R247-PERF-18 (#823): the assistant text turn must appear
// at index nextIdx, followed by tool_use turns at sequential indices —
// no prepend, no re-number. Before R247-PERF-2 the loop emitted tool_use
// turns first with provisional indices and then prepended the assistant
// turn via append([]turn{a}, out...) which forced a fresh backing slice
// allocation and an O(N) re-index per call. A future refactor that
// reintroduces the prepend pattern would break this index contract on
// the very first tool_use turn — assert it directly so the regression
// is loud at test time, not at dashboard-render time.
func TestFlattenAssistantEvent_AssistantFirstThenSequentialIndices(t *testing.T) {
	t.Parallel()

	// Mixed: text + 2 tool_use + text (text blocks merge, so we expect
	// 1 assistant turn + 2 tool_use turns = 3 turns total).
	ev := &claudeJSONLEvent{
		Type: "assistant",
		Message: json.RawMessage(`{"role":"assistant","content":[` +
			`{"type":"text","text":"first"},` +
			`{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"a"}},` +
			`{"type":"tool_use","id":"tu_2","name":"Read","input":{"file_path":"/x"}},` +
			`{"type":"text","text":"second"}` +
			`]}`),
	}
	const startIdx = 7 // arbitrary non-zero base to catch off-by-one
	out, _, toolCalls, parsed := flattenAssistantEvent(ev, 1234, startIdx)
	if !parsed {
		t.Fatalf("expected parsed=true")
	}
	if got, want := len(out), 3; got != want {
		t.Fatalf("len(out)=%d, want %d (1 assistant + 2 tool_use)", got, want)
	}
	if toolCalls != 2 {
		t.Errorf("toolCalls=%d, want 2", toolCalls)
	}

	// Index contract: assistant first at nextIdx, tool_use next at
	// nextIdx+1, nextIdx+2.
	if out[0].Kind != "assistant" {
		t.Errorf("out[0].Kind=%q, want %q (assistant must appear first; prepend regression)", out[0].Kind, "assistant")
	}
	if out[0].Index != startIdx {
		t.Errorf("out[0].Index=%d, want %d (assistant must land at nextIdx without re-number)", out[0].Index, startIdx)
	}
	if out[1].Kind != "tool_use" || out[1].Index != startIdx+1 {
		t.Errorf("out[1] = (%q, %d), want (tool_use, %d)", out[1].Kind, out[1].Index, startIdx+1)
	}
	if out[2].Kind != "tool_use" || out[2].Index != startIdx+2 {
		t.Errorf("out[2] = (%q, %d), want (tool_use, %d)", out[2].Kind, out[2].Index, startIdx+2)
	}

	// Tool-only branch: no text blocks, only tool_use. The assistant
	// turn must NOT be emitted at all (text empty), and tool_use turns
	// must start at nextIdx without a phantom slot reserved for the
	// missing assistant turn.
	toolOnly := &claudeJSONLEvent{
		Type: "assistant",
		Message: json.RawMessage(`{"role":"assistant","content":[` +
			`{"type":"tool_use","id":"tu_a","name":"Bash","input":{"command":"a"}},` +
			`{"type":"tool_use","id":"tu_b","name":"Read","input":{"file_path":"/y"}}` +
			`]}`),
	}
	out2, _, _, parsed2 := flattenAssistantEvent(toolOnly, 0, startIdx)
	if !parsed2 || len(out2) != 2 {
		t.Fatalf("tool-only: parsed=%v len(out)=%d (want true / 2)", parsed2, len(out2))
	}
	if out2[0].Kind != "tool_use" || out2[0].Index != startIdx {
		t.Errorf("tool-only out[0] = (%q, %d), want (tool_use, %d) — no phantom assistant slot",
			out2[0].Kind, out2[0].Index, startIdx)
	}
	if out2[1].Kind != "tool_use" || out2[1].Index != startIdx+1 {
		t.Errorf("tool-only out[1] = (%q, %d), want (tool_use, %d)",
			out2[1].Kind, out2[1].Index, startIdx+1)
	}
}

// TestTranscript_ConcurrencyCap_503WhenAllSlotsBusy pins R243-SEC-12 (#798):
// when transcriptSem is saturated (every transcriptConcurrencyCap slot
// held by an in-flight handler), arriving requests must receive 503
// "transcript busy" immediately — NOT 200 with a partial payload, and
// NOT a queued wait. The non-blocking acquire is the load-shedding gate
// that bounds peak resident bytes (cap × 8 MB LimitReader + cap ×
// 256 KB Scanner buffer) under multi-operator load.
//
// Test approach: directly fill the package-level transcriptSem channel
// to the cap, then call the handler with a known-good fixture and
// verify it returns 503. We restore the channel state via defer so
// subsequent parallel tests don't observe a saturated semaphore. NOT
// t.Parallel() because this test mutates package-global state.
func TestTranscript_ConcurrencyCap_503WhenAllSlotsBusy(t *testing.T) {
	// Saturate the package-level semaphore so the handler's non-blocking
	// acquire takes the default branch.
	for i := 0; i < transcriptConcurrencyCap; i++ {
		transcriptSem <- struct{}{}
	}
	// Drain on exit so other tests aren't blocked by leaked slots.
	defer func() {
		for i := 0; i < transcriptConcurrencyCap; i++ {
			<-transcriptSem
		}
	}()

	now := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339Nano)
	h, jobID, runID, _ := fixtureRunWithJSONL(t, []string{
		`{"type":"user","timestamp":"` + now + `","message":{"role":"user","content":"x"}}`,
	})

	w := callTranscript(h, jobID, runID)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("saturated semaphore must yield 503, got %d body=%s", w.Code, w.Body.String())
	}
	// Confirm the body shape matches the documented 503 envelope so
	// dashboard JS keeps its existing 5xx-fallback branch wired.
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal 503 body: %v body=%s", err, w.Body.String())
	}
	if got := body["error"]; got != "transcript busy" {
		t.Errorf("503 error field = %q, want %q", got, "transcript busy")
	}
}

// TestTranscript_ConcurrencyCap_ReleasesSlotOnReturn pins the defer-release
// contract: handleRunTranscript must release its semaphore slot on every
// return path so a transient burst doesn't permanently shrink the
// transcriptConcurrencyCap budget. We acquire cap-1 slots manually,
// run a real request through the handler (which acquires the last slot
// and must release it on return), then verify a follow-up request can
// still acquire — i.e. the slot count returned to capacity. NOT
// t.Parallel() because this test mutates package-global state.
func TestTranscript_ConcurrencyCap_ReleasesSlotOnReturn(t *testing.T) {
	// Hold cap-1 slots so the handler under test takes the LAST slot.
	for i := 0; i < transcriptConcurrencyCap-1; i++ {
		transcriptSem <- struct{}{}
	}
	defer func() {
		for i := 0; i < transcriptConcurrencyCap-1; i++ {
			<-transcriptSem
		}
	}()

	now := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339Nano)
	h, jobID, runID, _ := fixtureRunWithJSONL(t, []string{
		`{"type":"user","timestamp":"` + now + `","message":{"role":"user","content":"hi"}}`,
	})

	// First request must succeed (cap-1 held + 1 acquired = cap, no overflow).
	w := callTranscript(h, jobID, runID)
	if w.Code != http.StatusOK {
		t.Fatalf("first call status = %d body=%s", w.Code, w.Body.String())
	}

	// After the handler returns, its slot must be back in the pool.
	// Confirm by attempting a non-blocking acquire — if the slot is
	// available, this select takes the case branch.
	select {
	case transcriptSem <- struct{}{}:
		// Got the released slot back. Return it so we don't leak across
		// tests; the deferred drain above only counts cap-1 receives.
		<-transcriptSem
	default:
		t.Errorf("handler did not release its semaphore slot on return — "+
			"defer release contract broken; transcriptSem at cap=%d", len(transcriptSem))
	}
}
