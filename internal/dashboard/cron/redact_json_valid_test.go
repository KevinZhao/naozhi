package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// The secret/path redactors run over raw JSON in two dashboard handlers. When
// a redaction lands next to a JSON structural byte the naive text substitution
// used to yield invalid JSON, json.Encoder.Encode failed, and WriteJSON
// silently returned a 200 with an empty body — the transcript / run-events
// panels then rendered "no records". These tests pin the exact inputs that
// reproduced the bug.

func TestRedactToolInput_EnvAssignmentAtStringEndStaysValidJSON(t *testing.T) {
	t.Parallel()
	in := json.RawMessage(`{"command":"export ANTHROPIC_API_KEY=abc"}`)
	got := redactToolInput(in)
	if !json.Valid(got) {
		t.Fatalf("redactToolInput produced invalid JSON: %s", got)
	}
	if strings.Contains(string(got), "abc") {
		t.Errorf("secret leaked: %s", got)
	}
	var v map[string]any
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := v["command"].(string); !ok {
		t.Errorf("command key lost: %s", got)
	}
}

func TestRedactToolInput_InvalidRawFallsBackToPlaceholder(t *testing.T) {
	t.Parallel()
	// Not JSON at all AND contains a secret shape: whatever comes out must be
	// encodable so the enclosing response is never dropped.
	in := json.RawMessage(`{"command":"export ANTHROPIC_API_KEY=abc"`)
	got := redactToolInput(in)
	if !json.Valid(got) {
		t.Fatalf("fallback must be valid JSON, got %s", got)
	}
	if strings.Contains(string(got), "abc") {
		t.Errorf("secret leaked via fallback: %s", got)
	}
}

func TestTranscript_ToolInputWithEnvAssignmentStillServes200Body(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339Nano)
	lines := []string{
		`{"type":"assistant","timestamp":"` + now + `","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"export ANTHROPIC_API_KEY=abc"}}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
	}
	h, jobID, runID, _ := fixtureRunWithJSONL(t, lines)
	w := callTranscript(h, jobID, runID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if w.Body.Len() == 0 {
		t.Fatal("empty 200 body: transcript encoding silently failed")
	}
	var resp transcriptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if len(resp.Turns) != 1 || resp.Turns[0].Kind != "tool_use" {
		t.Fatalf("turns = %+v", resp.Turns)
	}
	if strings.Contains(string(resp.Turns[0].Input), "abc") {
		t.Errorf("secret leaked in input: %s", resp.Turns[0].Input)
	}
}

func TestHandleRunEvents_RedactionKeepsEveryLineValidJSON(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{
		StorePath:      filepath.Join(tmp, "cron_jobs.json"),
		AllowNilRouter: true,
	}, cronpkg.SchedulerDeps{})

	jobID := strings.Repeat("a", 16)
	runID := strings.Repeat("b", 16)
	evDir := filepath.Join(tmp, "sandboxevents", jobID)
	if err := os.MkdirAll(evDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lines := `{"kind":"cli","line":{"command":"export ANTHROPIC_API_KEY=abc"}}` + "\n" +
		`{"kind":"cli","cmd":"cat \"/tmp/f\""}` + "\n" +
		`{"kind":"exit","code":0}` + "\n"
	if err := os.WriteFile(filepath.Join(evDir, runID+".ndjson"), []byte(lines), 0o600); err != nil {
		t.Fatalf("write events: %v", err)
	}

	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/"+runID+"/events?job_id="+jobID, nil)
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	h.HandleRunEvents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if w.Body.Len() == 0 {
		t.Fatal("empty 200 body: run events encoding silently failed")
	}
	var resp struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if len(resp.Events) != 3 {
		t.Fatalf("events = %d, want 3", len(resp.Events))
	}
	for i, ev := range resp.Events {
		if !json.Valid(ev) {
			t.Errorf("event[%d] invalid JSON: %s", i, ev)
		}
		if strings.Contains(string(ev), "abc") {
			t.Errorf("event[%d] leaked secret: %s", i, ev)
		}
		if strings.Contains(string(ev), "/tmp/f") {
			t.Errorf("event[%d] leaked path: %s", i, ev)
		}
	}
	if !strings.Contains(string(resp.Events[2]), `"exit"`) {
		t.Errorf("clean trailing event lost: %s", resp.Events[2])
	}
}
