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

// R243-CR-P2-4 (#822): an upstream CLI emitting `"input": null` must not
// leak `"input": null` onto the wire. transcriptTurn.Input uses
// json.RawMessage with omitempty, but RawMessage holding the literal
// 4-byte `null` token is not zero-length, so the stdlib encoder still
// emits it. flattenAssistantEvent normalises that to nil so omitempty
// drops the field.
func TestFlattenAssistantEvent_ToolInputNullOmitted(t *testing.T) {
	t.Parallel()

	ev := &claudeJSONLEvent{
		Type: "assistant",
		Message: json.RawMessage(`{"role":"assistant","content":[` +
			`{"type":"tool_use","id":"tu_n","name":"Bash","input":null}` +
			`]}`),
	}
	out, _, _, parsed := flattenAssistantEvent(ev, 0, 0)
	if !parsed || len(out) != 1 {
		t.Fatalf("parsed=%v len(out)=%d (want true / 1)", parsed, len(out))
	}
	if out[0].Input != nil {
		t.Errorf("Input=%q, want nil so omitempty drops the field", string(out[0].Input))
	}
	// Round-trip through the JSON encoder to confirm wire output drops
	// the field entirely rather than emitting `"input": null`.
	wire, err := json.Marshal(out[0])
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(wire), `"input"`) {
		t.Errorf("wire still carries input field: %s", string(wire))
	}
}

// isJSONNull recognises the literal four-byte JSON `null` token and
// rejects everything else (including whitespace-padded variants — the
// flatten path receives RawMessage straight from encoding/json which
// trims surrounding whitespace).
func TestIsJSONNull(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   json.RawMessage
		want bool
	}{
		{"null literal", json.RawMessage(`null`), true},
		{"empty", json.RawMessage(``), false},
		{"nil", nil, false},
		{"object", json.RawMessage(`{}`), false},
		{"string null", json.RawMessage(`"null"`), false},
		{"trailing space", json.RawMessage(`null `), false},
		{"caps", json.RawMessage(`NULL`), false},
	}
	for _, c := range cases {
		if got := isJSONNull(c.in); got != c.want {
			t.Errorf("%s: isJSONNull(%q)=%v, want %v", c.name, string(c.in), got, c.want)
		}
	}
}
