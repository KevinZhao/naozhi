package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A fresh=false job's JSONL holds every earlier run. Once it passes the read
// budget, the newest run's turns sit past it; reading from the head left the
// run with no turns and the drawer fell back to raw. Later runs follow it in
// the same file.
func TestTranscript_FreshFalse_RunPastByteBudget(t *testing.T) {
	t.Parallel()
	old := time.Now().UTC().Add(-time.Hour)
	filler := strings.Repeat("x", 4000)
	var lines []string
	var size int64
	for i := 0; size < maxTranscriptBytes+maxTranscriptBytes/4; i++ {
		ts := old.Add(time.Duration(i) * time.Millisecond).Format(time.RFC3339Nano)
		l := `{"type":"assistant","timestamp":"` + ts + `","message":{"role":"assistant","content":[{"type":"text","text":"EARLIER_RUN ` + filler + `"}]}}`
		lines = append(lines, l)
		size += int64(len(l) + 1)
	}
	now := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339Nano)
	lines = append(lines,
		`{"type":"user","timestamp":"`+now+`","message":{"role":"user","content":"THIS_RUN prompt"}}`,
		`{"type":"assistant","timestamp":"`+now+`","message":{"role":"assistant","content":[{"type":"text","text":"THIS_RUN answer"}]}}`,
	)
	later := time.Now().UTC().Add(time.Hour)
	for i := 0; i < 500; i++ {
		ts := later.Add(time.Duration(i) * time.Millisecond).Format(time.RFC3339Nano)
		lines = append(lines, `{"type":"assistant","timestamp":"`+ts+`","message":{"role":"assistant","content":[{"type":"text","text":"LATER_RUN `+filler+`"}]}}`)
	}

	h, jobID, runID, _ := fixtureRunWithJSONLFresh(t, false, lines)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/"+runID+"/transcript?job_id="+jobID, nil)
	req.SetPathValue("run_id", runID)
	h.HandleRunTranscript(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp transcriptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Fallback != "" {
		t.Errorf("fallback = %q, want none", resp.Fallback)
	}
	got := 0
	for _, tr := range resp.Turns {
		if strings.Contains(tr.Text, "EARLIER_RUN") || strings.Contains(tr.Text, "LATER_RUN") {
			t.Fatalf("another run's turn leaked into this run")
		}
		if strings.Contains(tr.Text, "THIS_RUN") {
			got++
		}
	}
	if got != 2 {
		t.Errorf("this run's turns = %d, want 2 (turns=%d truncated=%v reason=%q)", got, len(resp.Turns), resp.Truncated, resp.TruncateReason)
	}
}
