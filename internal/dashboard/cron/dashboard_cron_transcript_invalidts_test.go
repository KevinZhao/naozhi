package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestTranscript_FreshTrue_DropsInvalidTimestamp pins R250-SEC-8 (#1097):
// even on the fresh=true branch (this run owns its JSONL exclusively),
// an event whose `timestamp` field is non-empty but unparseable must be
// dropped rather than included with ts=0. The pre-fix branch only
// skipped fresh=false events on parse failure; for fresh=true an
// operator with workspace write access could craft `timestamp:"bad"`
// JSONL lines that the transcript drawer would surface across every
// run. This test locks the post-fix policy: invalid timestamp →
// continue, regardless of fresh flag.
func TestTranscript_FreshTrue_DropsInvalidTimestamp(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339Nano)

	lines := []string{
		`{"type":"user","timestamp":"` + now + `","message":{"role":"user","content":"valid prompt"}}`,
		// Hostile entry: non-empty but unparseable timestamp. Pre-fix
		// this would slip through as ts=0 on the fresh=true path.
		`{"type":"assistant","timestamp":"not-a-timestamp","message":{"role":"assistant","content":[{"type":"text","text":"INJECTED_PAYLOAD"}]}}`,
		// Legitimate timestamp-less event must still flow on fresh=true.
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"FRESH_KEEPS_THIS"}]}}`,
	}

	w := httptest.NewRecorder()
	h, jobID, runID, _ := fixtureRunWithJSONLFresh(t, true, lines)
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/"+runID+"/transcript?job_id="+jobID, nil)
	req.SetPathValue("run_id", runID)
	h.HandleRunTranscript(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp transcriptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	for _, tr := range resp.Turns {
		if strings.Contains(tr.Text, "INJECTED_PAYLOAD") {
			t.Errorf("invalid-timestamp event surfaced in transcript — #1097 regression; turns=%+v", resp.Turns)
		}
	}
	keptUndated := false
	for _, tr := range resp.Turns {
		if strings.Contains(tr.Text, "FRESH_KEEPS_THIS") {
			keptUndated = true
		}
	}
	if !keptUndated {
		t.Errorf("legit timestamp-less event dropped — #1046 over-correction regression; turns=%+v", resp.Turns)
	}
}
