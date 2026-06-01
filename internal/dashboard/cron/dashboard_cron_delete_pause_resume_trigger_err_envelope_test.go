package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// TestCronDeletePauseResumeTrigger_JSONEnvelope pins R112714-SEC-1: the 4xx
// error paths of HandleDelete, HandlePause, HandleResume, and HandleTrigger
// must emit JSON {"error":"..."} (via writeCronErr), NOT text/plain (http.Error).
//
// Before the fix these four handlers called http.Error directly while
// HandleCreate/HandleUpdate used writeCronErr, creating a mixed wire format
// the client had to branch on. This test asserts every validation and
// not-found 4xx path writes Content-Type: application/json with a parseable
// "error" field so the dashboard can read body.error uniformly.
func TestCronDeletePauseResumeTrigger_JSONEnvelope(t *testing.T) {
	t.Parallel()

	type tc struct {
		name       string
		method     string
		target     string
		body       string
		wantStatus int
		invoke     func(h *Handlers, w http.ResponseWriter, r *http.Request)
	}

	del := func(h *Handlers, w http.ResponseWriter, r *http.Request) { h.HandleDelete(w, r) }
	pause := func(h *Handlers, w http.ResponseWriter, r *http.Request) { h.HandlePause(w, r) }
	resume := func(h *Handlers, w http.ResponseWriter, r *http.Request) { h.HandleResume(w, r) }
	trigger := func(h *Handlers, w http.ResponseWriter, r *http.Request) { h.HandleTrigger(w, r) }

	cases := []tc{
		// HandleDelete validation paths.
		{
			name:       "delete/id_missing",
			method:     http.MethodDelete,
			target:     "/api/cron",
			wantStatus: http.StatusBadRequest,
			invoke:     del,
		},
		{
			name:       "delete/id_too_long",
			method:     http.MethodDelete,
			target:     "/api/cron?id=" + strings.Repeat("a", 100),
			wantStatus: http.StatusBadRequest,
			invoke:     del,
		},
		{
			name:       "delete/id_invalid_shape",
			method:     http.MethodDelete,
			target:     "/api/cron?id=not-hex!",
			wantStatus: http.StatusBadRequest,
			invoke:     del,
		},
		{
			name:       "delete/not_found",
			method:     http.MethodDelete,
			target:     "/api/cron?id=deadbeefdeadbeef",
			wantStatus: http.StatusNotFound,
			invoke:     del,
		},
		// HandlePause validation paths.
		{
			name:       "pause/id_missing",
			method:     http.MethodPost,
			target:     "/api/cron/pause",
			body:       `{}`,
			wantStatus: http.StatusBadRequest,
			invoke:     pause,
		},
		{
			name:       "pause/id_too_long",
			method:     http.MethodPost,
			target:     "/api/cron/pause",
			body:       `{"id":"` + strings.Repeat("a", 100) + `"}`,
			wantStatus: http.StatusBadRequest,
			invoke:     pause,
		},
		{
			name:       "pause/id_invalid_shape",
			method:     http.MethodPost,
			target:     "/api/cron/pause",
			body:       `{"id":"not-hex!"}`,
			wantStatus: http.StatusBadRequest,
			invoke:     pause,
		},
		{
			name:       "pause/not_found",
			method:     http.MethodPost,
			target:     "/api/cron/pause",
			body:       `{"id":"deadbeefdeadbeef"}`,
			wantStatus: http.StatusNotFound,
			invoke:     pause,
		},
		// HandleResume validation paths.
		{
			name:       "resume/id_missing",
			method:     http.MethodPost,
			target:     "/api/cron/resume",
			body:       `{}`,
			wantStatus: http.StatusBadRequest,
			invoke:     resume,
		},
		{
			name:       "resume/id_invalid_shape",
			method:     http.MethodPost,
			target:     "/api/cron/resume",
			body:       `{"id":"not-hex!"}`,
			wantStatus: http.StatusBadRequest,
			invoke:     resume,
		},
		{
			name:       "resume/not_found",
			method:     http.MethodPost,
			target:     "/api/cron/resume",
			body:       `{"id":"deadbeefdeadbeef"}`,
			wantStatus: http.StatusNotFound,
			invoke:     resume,
		},
		// HandleTrigger validation paths.
		{
			name:       "trigger/invalid_json",
			method:     http.MethodPost,
			target:     "/api/cron/trigger",
			body:       "{not json",
			wantStatus: http.StatusBadRequest,
			invoke:     trigger,
		},
		{
			name:       "trigger/id_missing",
			method:     http.MethodPost,
			target:     "/api/cron/trigger",
			body:       `{}`,
			wantStatus: http.StatusBadRequest,
			invoke:     trigger,
		},
		{
			name:       "trigger/id_invalid_shape",
			method:     http.MethodPost,
			target:     "/api/cron/trigger",
			body:       `{"id":"not-hex!"}`,
			wantStatus: http.StatusBadRequest,
			invoke:     trigger,
		},
		{
			name:       "trigger/not_found",
			method:     http.MethodPost,
			target:     "/api/cron/trigger",
			body:       `{"id":"deadbeefdeadbeef"}`,
			wantStatus: http.StatusNotFound,
			invoke:     trigger,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := &Handlers{scheduler: cronpkg.NewScheduler(cronpkg.SchedulerConfig{AllowNilRouter: true})}

			var bodyReader *strings.Reader
			if c.body != "" {
				bodyReader = strings.NewReader(c.body)
			} else {
				bodyReader = strings.NewReader("")
			}
			req := httptest.NewRequest(c.method, c.target, bodyReader)
			req.Header.Set("Content-Type", "application/json")
			req.RemoteAddr = "127.0.0.1:9999"
			w := httptest.NewRecorder()

			c.invoke(h, w, req)

			if w.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d (raw body=%q)", w.Code, c.wantStatus, w.Body.String())
			}
			ct := w.Result().Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("Content-Type = %q, want application/json prefix — "+
					"error path must write JSON envelope, not text/plain [R112714-SEC-1]", ct)
			}
			var env struct {
				Error string `json:"error"`
			}
			if err := json.NewDecoder(w.Body).Decode(&env); err != nil {
				t.Fatalf("body is not JSON: %v; raw=%q", err, w.Body.String())
			}
			if strings.TrimSpace(env.Error) == "" {
				t.Errorf("error field empty; client cannot read body.error; raw=%q", w.Body.String())
			}
		})
	}
}
