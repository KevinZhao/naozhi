package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCronPreview_ValidationErr_JSONEnvelope pins R112714-SEC-6: the
// validation error paths of HandlePreview must emit JSON {"error":"..."}
// (via writeCronErr), NOT text/plain (http.Error).
//
// Before the fix, HandlePreview called http.Error for validation failures
// while the parse-failure and success paths wrote JSON — mixed wire formats
// requiring client-side Content-Type branching.
func TestCronPreview_ValidationErr_JSONEnvelope(t *testing.T) {
	t.Parallel()

	type tc struct {
		name       string
		target     string
		wantStatus int
	}

	cases := []tc{
		{
			name:       "schedule_missing",
			target:     "/api/cron/preview",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "schedule_too_long",
			target:     "/api/cron/preview?schedule=" + strings.Repeat("x", 300),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "schedule_invalid_chars",
			target:     "/api/cron/preview?schedule=@daily%00null",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "count_too_long",
			target:     "/api/cron/preview?schedule=@daily&count=9999",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "count_not_integer",
			target:     "/api/cron/preview?schedule=@daily&count=abc",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "count_zero",
			target:     "/api/cron/preview?schedule=@daily&count=0",
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := &Handlers{} // nil scheduler is fine; preview uses nil-safe method

			req := httptest.NewRequest(http.MethodGet, c.target, nil)
			req.RemoteAddr = "127.0.0.1:9999"
			w := httptest.NewRecorder()

			h.HandlePreview(w, req)

			if w.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d (raw body=%q)", w.Code, c.wantStatus, w.Body.String())
			}
			ct := w.Result().Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("Content-Type = %q, want application/json prefix — "+
					"validation error path must write JSON, not text/plain [R112714-SEC-6]", ct)
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
