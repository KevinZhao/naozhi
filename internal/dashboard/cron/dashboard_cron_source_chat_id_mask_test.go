package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// TestHandleList_SourceChatIDMasked pins R090135-LOGIC-2: the per-job
// source ChatID (IM origin, e.g. oc_sourcechat…) must be masked in the
// GET /api/cron list response, matching the treatment already applied to
// NotifyChatID. PR #1728 fixed NotifyChatID but explicitly left ChatID
// unmasked; that exclusion was incorrect in a multi-operator deployment
// where authenticated dashboard users may not all have visibility into
// the source chat.
//
// Contract:
//   - resp.Jobs[0].ChatID must NOT equal the raw ChatID.
//   - resp.Jobs[0].ChatID must NOT contain the raw ChatID as a substring.
//   - resp.Jobs[0].ChatID must contain the "…" ellipsis affordance.
func TestHandleList_SourceChatIDMasked(t *testing.T) {
	t.Parallel()

	const rawChatID = "oc_sourcechat12345"

	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{})
	if err := sched.AddJob(&cronpkg.Job{
		ID:       "cc00000000000001",
		Schedule: "*/5 * * * *",
		Prompt:   "test prompt",
		Platform: "feishu",
		ChatID:   rawChatID,
	}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet, "/api/cron", nil)
	w := httptest.NewRecorder()
	h.HandleList(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HandleList returned %d; body=%s", w.Code, w.Body.String())
	}

	var resp cronListResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
	}
	if len(resp.Jobs) != 1 {
		t.Fatalf("want 1 job in response, got %d", len(resp.Jobs))
	}

	got := resp.Jobs[0].ChatID

	// Must not be returned verbatim — source chat ID leak.
	if got == rawChatID {
		t.Errorf("chat_id returned verbatim (%q) — source chat ID leaked [R090135-LOGIC-2]", got)
	}
	if strings.Contains(got, rawChatID) {
		t.Errorf("chat_id %q still contains the full raw ID %q", got, rawChatID)
	}

	// Must carry the ellipsis affordance (long ID → prefix…suffix form).
	if !strings.Contains(got, "…") {
		t.Errorf("chat_id %q missing ellipsis affordance — UI hint broken", got)
	}

	// Must preserve the 4-rune prefix so the UI can display e.g. "oc_s…5678".
	r := []rune(rawChatID)
	if !strings.HasPrefix(got, string(r[:4])) {
		t.Errorf("chat_id %q lost 4-rune prefix hint (want prefix %q)", got, string(r[:4]))
	}
}
