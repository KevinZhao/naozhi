package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// TestCronMutationErr_JSONEnvelope pins R20260531-SEC-8 (#1518): the cron
// create/update validation 4xx paths used to call http.Error (text/plain)
// while the success and persist-failure paths wrote JSON. dashboard.js then
// had to branch on Content-Type to read the error. After the fix every cron
// mutation error path flows through writeCronErr → httputil.WriteJSONStatus,
// so the client can read body.error uniformly.
//
// For each rejected request we assert:
//   - the expected 4xx/5xx status (behaviour preserved), and
//   - Content-Type is application/json (NOT text/plain), and
//   - the body parses as JSON with a non-empty "error" field.
//
// A regression that reverts any of these sites to http.Error trips the
// Content-Type / JSON-decode assertions.
func TestCronMutationErr_JSONEnvelope(t *testing.T) {
	t.Parallel()

	type tc struct {
		name       string
		method     string
		target     string // path (+query)
		body       string // JSON request body ("" => no body)
		wantStatus int
		invoke     func(h *Handlers, w http.ResponseWriter, r *http.Request)
	}

	create := func(h *Handlers, w http.ResponseWriter, r *http.Request) { h.HandleCreate(w, r) }
	update := func(h *Handlers, w http.ResponseWriter, r *http.Request) { h.HandleUpdate(w, r) }

	cases := []tc{
		// HandleCreate validation paths.
		{
			name:       "create/invalid_json",
			method:     http.MethodPost,
			target:     "/api/cron",
			body:       "{not json",
			wantStatus: http.StatusBadRequest,
			invoke:     create,
		},
		{
			name:       "create/schedule_required",
			method:     http.MethodPost,
			target:     "/api/cron",
			body:       `{"prompt":"hi"}`,
			wantStatus: http.StatusBadRequest,
			invoke:     create,
		},
		{
			name:       "create/notify_half_set",
			method:     http.MethodPost,
			target:     "/api/cron",
			body:       `{"schedule":"@daily","notify_platform":"feishu"}`,
			wantStatus: http.StatusBadRequest,
			invoke:     create,
		},
		// HandleUpdate validation paths.
		{
			name:       "update/id_required",
			method:     http.MethodPatch,
			target:     "/api/cron",
			body:       `{"prompt":"x"}`,
			wantStatus: http.StatusBadRequest,
			invoke:     update,
		},
		{
			name:       "update/invalid_id",
			method:     http.MethodPatch,
			target:     "/api/cron?id=not-hex",
			body:       `{"prompt":"x"}`,
			wantStatus: http.StatusBadRequest,
			invoke:     update,
		},
		{
			name:       "update/no_fields",
			method:     http.MethodPatch,
			target:     "/api/cron?id=0123456789abcdef",
			body:       `{}`,
			wantStatus: http.StatusBadRequest,
			invoke:     update,
		},
		{
			name:       "update/notify_patched_apart",
			method:     http.MethodPatch,
			target:     "/api/cron?id=0123456789abcdef",
			body:       `{"notify_platform":"feishu"}`,
			wantStatus: http.StatusUnprocessableEntity,
			invoke:     update,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := &Handlers{scheduler: cronpkg.NewScheduler(cronpkg.SchedulerConfig{})}

			var bodyReader *strings.Reader
			if c.body != "" {
				bodyReader = strings.NewReader(c.body)
			} else {
				bodyReader = strings.NewReader("")
			}
			req := httptest.NewRequest(c.method, c.target, bodyReader)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			c.invoke(h, w, req)

			if w.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d (raw body=%q)", w.Code, c.wantStatus, w.Body.String())
			}
			ct := w.Result().Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("Content-Type = %q, want application/json prefix — error path must write JSON envelope, not text/plain", ct)
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
