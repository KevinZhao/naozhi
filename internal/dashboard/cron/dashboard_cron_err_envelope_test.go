package cron

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// TestCronWriteHandlers_ErrEnvelope is the single table for the cron write
// handlers' error paths. Every row must answer with the expected status and
// the JSON {"error":"..."} envelope (writeCronErr), never text/plain
// (R112714-SEC-1, R20260531-SEC-8 #1518).
//
// Rows flagged noLog carry an id with a newline: the shape gate must reject
// it with 400 before the bytes reach the scheduler or slog (R250-SEC-1 log
// injection). The "notify half-set" create rows pin R242-SEC-11 (#640): either
// side of the notify target alone must be rejected.
//
// Not t.Parallel: the test swaps slog.Default (process-global).
func TestCronWriteHandlers_ErrEnvelope(t *testing.T) {
	const badID = "abc\nfake-log-line"
	if cronpkg.IsValidID(badID) {
		t.Fatalf("fixture %q unexpectedly valid", badID)
	}
	idBody := func(id string) string {
		b, _ := json.Marshal(map[string]string{"id": id})
		return string(b)
	}
	badQuery := "?id=" + url.QueryEscape(badID)

	del := func(h *Handlers, w http.ResponseWriter, r *http.Request) { h.HandleDelete(w, r) }
	pause := func(h *Handlers, w http.ResponseWriter, r *http.Request) { h.HandlePause(w, r) }
	resume := func(h *Handlers, w http.ResponseWriter, r *http.Request) { h.HandleResume(w, r) }
	trigger := func(h *Handlers, w http.ResponseWriter, r *http.Request) { h.HandleTrigger(w, r) }
	create := func(h *Handlers, w http.ResponseWriter, r *http.Request) { h.HandleCreate(w, r) }
	update := func(h *Handlers, w http.ResponseWriter, r *http.Request) { h.HandleUpdate(w, r) }

	cases := []struct {
		name       string
		method     string
		target     string
		body       string
		invoke     func(h *Handlers, w http.ResponseWriter, r *http.Request)
		wantStatus int
		wantErr    string // substring of body.error; "" = any non-empty
		noLog      bool
	}{
		{"delete/id_missing", http.MethodDelete, "/api/cron", "", del, 400, "", false},
		{"delete/bad_id", http.MethodDelete, "/api/cron" + badQuery, "", del, 400, "", true},
		{"delete/not_found", http.MethodDelete, "/api/cron?id=deadbeefdeadbeef", "", del, 404, "", false},
		{"pause/id_too_long", http.MethodPost, "/api/cron/pause", idBody(strings.Repeat("a", 100)), pause, 400, "", false},
		{"pause/bad_id", http.MethodPost, "/api/cron/pause", idBody(badID), pause, 400, "", true},
		{"pause/not_found", http.MethodPost, "/api/cron/pause", idBody("deadbeefdeadbeef"), pause, 404, "", false},
		{"resume/bad_id", http.MethodPost, "/api/cron/resume", idBody(badID), resume, 400, "", true},
		{"resume/not_found", http.MethodPost, "/api/cron/resume", idBody("deadbeefdeadbeef"), resume, 404, "", false},
		{"trigger/invalid_json", http.MethodPost, "/api/cron/trigger", "{not json", trigger, 400, "", false},
		{"trigger/bad_id", http.MethodPost, "/api/cron/trigger", idBody(badID), trigger, 400, "", true},
		{"trigger/not_found", http.MethodPost, "/api/cron/trigger", idBody("deadbeefdeadbeef"), trigger, 404, "", false},
		{"create/invalid_json", http.MethodPost, "/api/cron", "{not json", create, 400, "", false},
		{"create/schedule_required", http.MethodPost, "/api/cron", `{"prompt":"hi"}`, create, 400, "", false},
		{"create/notify_platform_only", http.MethodPost, "/api/cron",
			`{"schedule":"* * * * *","prompt":"hi","notify_platform":"feishu","notify_chat_id":""}`, create, 400, "must be set together", false},
		{"create/notify_chatid_only", http.MethodPost, "/api/cron",
			`{"schedule":"* * * * *","prompt":"hi","notify_platform":"","notify_chat_id":"oc_xxxxxxxxxxxxxxxx"}`, create, 400, "must be set together", false},
		{"update/id_required", http.MethodPatch, "/api/cron", `{"prompt":"x"}`, update, 400, "", false},
		{"update/bad_id", http.MethodPatch, "/api/cron" + badQuery, `{"prompt":"x"}`, update, 400, "", true},
		{"update/no_fields", http.MethodPatch, "/api/cron?id=0123456789abcdef", `{}`, update, 400, "", false},
		{"update/notify_patched_apart", http.MethodPatch, "/api/cron?id=0123456789abcdef", `{"notify_platform":"feishu"}`, update, 422, "", false},
	}

	h := &Handlers{deps: Deps{Scheduler: cronpkg.NewScheduler(cronpkg.SchedulerConfig{AllowNilRouter: true}, cronpkg.SchedulerDeps{})}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			defer slog.SetDefault(prev)

			req := httptest.NewRequest(c.method, c.target, strings.NewReader(c.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			c.invoke(h, w, req)

			if w.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d (body=%q)", w.Code, c.wantStatus, w.Body.String())
			}
			if ct := w.Result().Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("Content-Type = %q, want application/json envelope", ct)
			}
			var env struct {
				Error string `json:"error"`
			}
			if err := json.NewDecoder(w.Body).Decode(&env); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			if strings.TrimSpace(env.Error) == "" || !strings.Contains(env.Error, c.wantErr) {
				t.Errorf("error = %q, want non-empty containing %q", env.Error, c.wantErr)
			}
			if c.noLog && logBuf.Len() != 0 {
				t.Errorf("invalid id produced log output (log-injection guard): %q", logBuf.String())
			}
		})
	}

	// httpErrPersistFailed is the shared 500 path for ErrPersistFailed
	// (R20260527-ARCH-2 #1274): JSON envelope with the stable code token.
	t.Run("persist_failed", func(t *testing.T) {
		w := httptest.NewRecorder()
		httpErrPersistFailed(w, "deleted")
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
		if ct := w.Result().Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("Content-Type = %q, want application/json", ct)
		}
		var body struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Code != "persist_failed" || !strings.Contains(body.Error, "deleted") || !strings.Contains(body.Error, "not persisted") {
			t.Errorf("body = %+v, want code=persist_failed and error naming the verb + \"not persisted\"", body)
		}
	})
}
