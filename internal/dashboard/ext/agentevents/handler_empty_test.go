package agentevents

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAgentEvents_EmptyTranscriptReturnsEmptyArray: a resolved task whose
// jsonl has no entries yet must serialise as `[]`, not `null`. agent_view.js
// treats a falsy body as "no data" and never subscribes to the live WS feed,
// leaving a permanent spinner.
func TestAgentEvents_EmptyTranscriptReturnsEmptyArray(t *testing.T) {
	h := seededHandler(t)

	w := httptest.NewRecorder()
	h.HandleAgentEvents(w, agentEventsReq(testAgentEventsKey, "t1", "", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if body := strings.TrimSpace(w.Body.String()); body != "[]" {
		t.Fatalf("body = %q, want []", body)
	}
}
