package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// TestHandleList_StripsIMIdentifiers pins R20260531A-SEC-1 (#1498): the list
// endpoint must not expose ChatID or NotifyChatID to all authenticated users
// in a multi-operator deployment. Both fields contain IM-channel identifiers
// scoped to the job owner; any authenticated caller can hit the list endpoint,
// so leaking them allows cross-user IM-ID enumeration.
//
// The fix zeroes both fields in the list view; they remain available via the
// create/update handlers which require explicit write intent.
func TestHandleList_StripsIMIdentifiers(t *testing.T) {
	t.Parallel()

	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{})
	if err := sched.AddJob(&cronpkg.Job{
		Schedule:     "*/5 * * * *",
		Prompt:       "hello",
		Platform:     "feishu",
		ChatID:       "oc_secret_chat_id",
		NotifyChatID: "oc_secret_notify_id",
	}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet, "/api/cron", nil)
	w := httptest.NewRecorder()
	h.HandleList(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}

	var got cronListResp
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if len(got.Jobs) != 1 {
		t.Fatalf("want 1 job, got %d", len(got.Jobs))
	}

	j := got.Jobs[0]
	if j.ChatID != "" {
		t.Errorf("list response leaked ChatID=%q — must be empty (R20260531A-SEC-1)", j.ChatID)
	}
	if j.NotifyChatID != "" {
		t.Errorf("list response leaked NotifyChatID=%q — must be empty (R20260531A-SEC-1)", j.NotifyChatID)
	}

	// Sanity: non-sensitive fields are still present so the list remains usable.
	if j.Schedule != "*/5 * * * *" {
		t.Errorf("Schedule missing from list response: got %q", j.Schedule)
	}
	if j.Platform != "feishu" {
		t.Errorf("Platform missing from list response: got %q", j.Platform)
	}

	// Belt-and-suspenders: scan raw JSON body so a future refactor that adds a
	// new field path cannot re-introduce the leak without this assertion firing.
	body := w.Body.String()
	if strings.Contains(body, "oc_secret_chat_id") {
		t.Errorf("raw JSON body contains oc_secret_chat_id — field was not stripped")
	}
	if strings.Contains(body, "oc_secret_notify_id") {
		t.Errorf("raw JSON body contains oc_secret_notify_id — field was not stripped")
	}
}
