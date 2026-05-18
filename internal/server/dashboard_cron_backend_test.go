package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestValidateCronBackend_Standalone exercises the unit-level validator
// directly so the rule lives close to its implementation. The HTTP-level
// tests below additionally pin the boundary contract — together they
// guard against either layer drifting (e.g. someone removing the handler
// call but leaving the validator, or vice versa).
func TestValidateCronBackend_Standalone(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"empty_ok", "", false},
		{"lowercase", "claude", false},
		{"with_digits", "claude4", false},
		{"with_underscore", "kiro_v2", false},
		{"with_hyphen", "long-name-1", false},
		{"max_len_32", strings.Repeat("a", 32), false},
		{"over_max_len", strings.Repeat("a", 33), true},
		// Send.go's gate matches [a-z0-9_-]; everything else is hostile.
		{"uppercase_rejected", "Claude", true},
		{"space_rejected", "claude ", true},
		{"dot_rejected", "claude.v2", true},
		{"slash_rejected", "claude/v2", true},
		{"newline_rejected", "claude\n", true},
		{"control_byte_rejected", "claude\x00", true},
		{"unicode_rejected", "克劳德", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateCronBackend(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateCronBackend(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

// postCronCreate is a small helper that exercises the full HTTP
// pipeline (mux + auth wrapper + handler) for POST /api/cron. Returning
// the response so each test can assert on status + body keeps the body
// reusable across the accept / reject tests below.
func postCronCreate(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/cron", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	return w
}

// TestCronCreate_AcceptsValidBackend pins the happy path for Sprint 6c:
// a well-formed dashboard payload with an explicit backend lands a job
// in the scheduler and the persisted shape carries the backend through
// to the list response. The minimum cron interval is 5m so we use
// "@every 5m" — anything shorter trips validateSchedule.
func TestCronCreate_AcceptsValidBackend(t *testing.T) {
	t.Parallel()
	srv := newTestServerWithScheduler(&mockPlatform{})

	payload := map[string]any{
		"schedule": "@every 5m",
		"prompt":   "hello from cron",
		"backend":  "kiro",
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	w := postCronCreate(t, srv, string(raw))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// List the jobs through the same HTTP pipeline so the assertion runs
	// against the wire shape the dashboard actually consumes.
	listReq := httptest.NewRequest(http.MethodGet, "/api/cron", nil)
	listW := httptest.NewRecorder()
	srv.mux.ServeHTTP(listW, listReq)
	if listW.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body=%s", listW.Code, listW.Body.String())
	}
	var listResp struct {
		Jobs []struct {
			ID      string `json:"id"`
			Backend string `json:"backend"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(listW.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listResp.Jobs) != 1 {
		t.Fatalf("listed jobs = %d, want 1", len(listResp.Jobs))
	}
	if got := listResp.Jobs[0].Backend; got != "kiro" {
		t.Errorf("listed backend = %q, want %q", got, "kiro")
	}
}

// TestCronCreate_RejectsInvalidBackendChars covers the regex-style
// boundary validator: any byte outside [a-z0-9_-] must be rejected at
// the dashboard edge with 400 so a multi-KB hostile `backend=` cannot
// land in slog or in cron_jobs.json. The cases mirror send.go's gate to
// keep the two surfaces in sync.
func TestCronCreate_RejectsInvalidBackendChars(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		val  string
	}{
		{"uppercase", "Claude"},
		{"dot", "claude.v2"},
		{"slash", "claude/v2"},
		{"space", "claude v2"},
		{"newline", "claude\n"},
		{"too_long", strings.Repeat("a", 33)},
		{"emoji", "claude🚀"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newTestServerWithScheduler(&mockPlatform{})
			payload := map[string]any{
				"schedule": "@every 5m",
				"prompt":   "x",
				"backend":  tc.val,
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			w := postCronCreate(t, srv, string(raw))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// TestCronCreate_AcceptsEmptyBackend pins the legacy / single-backend
// path: omitting backend (or sending "") MUST be accepted. This is the
// upgrade-day contract — old dashboards that never knew about backend
// keep working, and single-backend deploys (the renderBackendPicker
// collapse case) submit no field at all.
func TestCronCreate_AcceptsEmptyBackend(t *testing.T) {
	t.Parallel()
	srv := newTestServerWithScheduler(&mockPlatform{})

	// Two flavours: omitted entirely vs explicit empty string.
	cases := []struct {
		name string
		body string
	}{
		{
			name: "omitted",
			body: `{"schedule":"@every 5m","prompt":"x"}`,
		},
		{
			name: "explicit_empty",
			body: `{"schedule":"@every 5m","prompt":"x","backend":""}`,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Subtest is sequential because they share the scheduler;
			// running parallel would risk per-chat cron limit on the
			// "dashboard"/"global" key. Two cases is light enough that
			// the loss of parallelism doesn't matter.
			req := httptest.NewRequest(http.MethodPost, "/api/cron", bytes.NewReader([]byte(tc.body)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.mux.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// TestCronUpdate_AcceptsBackendChange pins the round-trip on the PATCH
// path: an existing job (created with backend=A) must accept a PATCH
// switching to backend=B, and the list endpoint must reflect the new
// value. This is the dashboard "I want this cron on Kiro instead" flow.
func TestCronUpdate_AcceptsBackendChange(t *testing.T) {
	t.Parallel()
	srv := newTestServerWithScheduler(&mockPlatform{})

	// Create with backend "claude".
	createBody := `{"schedule":"@every 5m","prompt":"x","backend":"claude"}`
	cw := postCronCreate(t, srv, createBody)
	if cw.Code != http.StatusOK {
		t.Fatalf("create status = %d; body=%s", cw.Code, cw.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(cw.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	// PATCH to backend "kiro".
	patchBody := `{"backend":"kiro"}`
	patchReq := httptest.NewRequest(http.MethodPatch, "/api/cron?id="+created.ID, strings.NewReader(patchBody))
	patchReq.Header.Set("Content-Type", "application/json")
	patchW := httptest.NewRecorder()
	srv.mux.ServeHTTP(patchW, patchReq)
	if patchW.Code != http.StatusOK {
		t.Fatalf("patch status = %d, want 200; body=%s", patchW.Code, patchW.Body.String())
	}

	// Read back through list to confirm.
	listReq := httptest.NewRequest(http.MethodGet, "/api/cron", nil)
	listW := httptest.NewRecorder()
	srv.mux.ServeHTTP(listW, listReq)
	var listResp struct {
		Jobs []struct {
			ID      string `json:"id"`
			Backend string `json:"backend"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(listW.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listResp.Jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(listResp.Jobs))
	}
	if got := listResp.Jobs[0].Backend; got != "kiro" {
		t.Errorf("backend after PATCH = %q, want %q", got, "kiro")
	}
}

// TestCronUpdate_RejectsInvalidBackend pins the negative path on PATCH:
// an existing job edit with a malformed backend must 400 and leave the
// stored value untouched (no partial write). Verifies the validator
// fires before UpdateJob commits anything.
func TestCronUpdate_RejectsInvalidBackend(t *testing.T) {
	t.Parallel()
	srv := newTestServerWithScheduler(&mockPlatform{})

	createBody := `{"schedule":"@every 5m","prompt":"x","backend":"claude"}`
	cw := postCronCreate(t, srv, createBody)
	if cw.Code != http.StatusOK {
		t.Fatalf("create status = %d; body=%s", cw.Code, cw.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(cw.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	patchBody := `{"backend":"BAD/CHARS"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/cron?id="+created.ID, strings.NewReader(patchBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}

	// Confirm the stored value still says "claude".
	listReq := httptest.NewRequest(http.MethodGet, "/api/cron", nil)
	listW := httptest.NewRecorder()
	srv.mux.ServeHTTP(listW, listReq)
	var listResp struct {
		Jobs []struct {
			Backend string `json:"backend"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(listW.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listResp.Jobs) != 1 || listResp.Jobs[0].Backend != "claude" {
		t.Fatalf("backend after rejected PATCH = %+v, want unchanged 'claude'", listResp.Jobs)
	}
}
