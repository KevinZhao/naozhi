package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// TestHandleList_PerJobNotifyChatIDMasked pins R20260604064416-SEC-1: the
// per-job NotifyChatID must never be surfaced verbatim in the GET /api/cron
// list response. Before this fix, cronJobView was constructed with
// `NotifyChatID: j.NotifyChatID` (raw), while maskNotifyChatID() was only
// applied to the global cron.notify_default.ChatID field. In a
// multi-operator deployment this exposed private Feishu oc_… open_ids to
// every authenticated dashboard user.
//
// Contract (mirrors TestMaskNotifyChatID but exercises the HTTP layer):
//   - The list response notify_chat_id for a job must NOT equal the raw ID.
//   - The masked value must contain the "…" affordance (prefix/suffix hint).
//   - The masked value must preserve the 4-rune prefix of the original ID.
//   - j.ChatID (the IM source chat) is also masked — R090135-LOGIC-2.
func TestHandleList_PerJobNotifyChatIDMasked(t *testing.T) {
	t.Parallel()

	const rawNotifyChatID = "oc_abcdefghijklmnop"

	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{})
	if err := sched.AddJob(&cronpkg.Job{
		ID:             "bb00000000000001",
		Schedule:       "*/5 * * * *",
		Prompt:         "test prompt",
		Platform:       "feishu",
		ChatID:         "oc_sourcechat",
		NotifyPlatform: "feishu",
		NotifyChatID:   rawNotifyChatID,
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

	got := resp.Jobs[0].NotifyChatID

	// Must not be the raw value — full-ID leak.
	if got == rawNotifyChatID {
		t.Errorf("notify_chat_id returned verbatim (%q) — per-job chat ID leaked", got)
	}
	if strings.Contains(got, rawNotifyChatID) {
		t.Errorf("notify_chat_id %q still contains the full raw ID %q", got, rawNotifyChatID)
	}

	// Must carry the ellipsis affordance (long ID → prefix…suffix form).
	if !strings.Contains(got, "…") {
		t.Errorf("notify_chat_id %q missing ellipsis affordance — UI hint broken", got)
	}

	// Must preserve the 4-rune prefix so the UI can display "oc_a…mnop".
	r := []rune(rawNotifyChatID)
	if !strings.HasPrefix(got, string(r[:4])) {
		t.Errorf("notify_chat_id %q lost 4-rune prefix hint (want prefix %q)", got, string(r[:4]))
	}

	// j.ChatID (IM source/origin chat) must also be masked — R090135-LOGIC-2.
	// The PATCH /api/cron flow never reads ChatID back from the list response
	// (update.go has no ChatID field in its request struct), so masking here
	// is safe and does not break any edit flow.
	gotChatID := resp.Jobs[0].ChatID
	const rawChatID = "oc_sourcechat"
	if gotChatID == rawChatID {
		t.Errorf("chat_id returned verbatim (%q) — origin IM chat ID leaked", gotChatID)
	}
	if !strings.Contains(gotChatID, "…") {
		t.Errorf("chat_id %q missing ellipsis affordance after masking", gotChatID)
	}
	rchat := []rune(rawChatID)
	if !strings.HasPrefix(gotChatID, string(rchat[:4])) {
		t.Errorf("chat_id %q lost 4-rune prefix hint (want prefix %q)", gotChatID, string(rchat[:4]))
	}
}
