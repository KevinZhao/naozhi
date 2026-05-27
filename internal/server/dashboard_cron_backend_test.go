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
		// R20260527122801-ARCH-8 (#1314): server cap aligned to 64 to match
		// session/router_backend.go's maxBackendBytes. The previous 32-byte
		// cap rejected legal 33–64 byte IDs that the router accepted, so a
		// dashboard editor saw "invalid backend" for an ID the cron path
		// happily routed.
		{"max_len_64", strings.Repeat("a", 64), false},
		{"over_max_len", strings.Repeat("a", 65), true},
		// R233-SEC-9: charset unified onto isValidBackendID
		// ([a-zA-Z0-9._-]) shared by HTTP send / WS dispatch / cron CRUD.
		// Uppercase + dot are now ACCEPTED here to match the WS path; the
		// older [a-z0-9_-] subset was only enforced on cron / send and led
		// to "valid on one surface, rejected on another" asymmetry.
		{"uppercase_ok_R233_SEC_9", "Claude", false},
		{"dot_ok_R233_SEC_9", "claude.v2", false},
		// Anything outside isValidBackendID's superset still rejected.
		{"space_rejected", "claude ", true},
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
// boundary validator: any byte outside isValidBackendID's superset
// ([a-zA-Z0-9._-]) must be rejected at the dashboard edge with 400 so
// a multi-KB hostile `backend=` cannot land in slog or in cron_jobs.json.
// Cases mirror send.go's gate to keep the surfaces in sync (R233-SEC-9).
//
// Uppercase + dot are NOT in this list anymore: R233-SEC-9 unified the
// cron / send / WS charsets onto isValidBackendID, which accepts
// [A-Za-z0-9._-]. The previous tighter [a-z0-9_-] cron-only rule rejected
// uppercase + dot — see TestValidateCronBackend_Standalone for the
// positive-case coverage of that relaxation.
func TestCronCreate_RejectsInvalidBackendChars(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		val  string
	}{
		{"slash", "claude/v2"},
		{"space", "claude v2"},
		{"newline", "claude\n"},
		{"too_long", strings.Repeat("a", 65)},
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

// TestCronUpdate_RejectsHalfNotifyPatch pins R238-SEC-14: a PATCH that
// touches ONE notify field but omits the other lands an orphan-target on
// disk (UpdateJob applies the present pointer in isolation and the missing
// pointer is interpreted as "leave existing"). Concrete failure: a job with
// {platform="feishu", chat_id="oc_x"} PATCHed with notify_platform:""
// alone clears the platform but leaves chat_id behind, silently re-routing
// notifications to cron.notify_default. Reject the half-PATCH with 422 so
// the dashboard fails fast instead of persisting the orphan tuple.
//
// The platformSet/chatIDSet check below this gate covers the (set, absent)
// shape; the new "both pointers must be present together" gate covers the
// (cleared, absent) shape that previously slipped through.
func TestCronUpdate_RejectsHalfNotifyPatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		body     string
		wantCode int
	}{
		// Only notify_platform present (clearing it) — notify_chat_id absent.
		// Caught by the new pointer-pair gate (422).
		{"clear_platform_only", `{"notify_platform":""}`, http.StatusUnprocessableEntity},
		// Only notify_chat_id present (clearing it) — notify_platform absent.
		// Caught by the new pointer-pair gate (422).
		{"clear_chat_id_only", `{"notify_chat_id":""}`, http.StatusUnprocessableEntity},
		// Only notify_platform set — notify_chat_id absent. Caught by the
		// new pointer-pair gate first (422). Asserting the unified contract:
		// half-PATCH always rejects regardless of set-vs-clear semantics.
		{"set_platform_only", `{"notify_platform":"feishu"}`, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newTestServerWithScheduler(&mockPlatform{})

			// Seed a job that already carries both notify halves so the
			// PATCH is exercising the "edit one half" hazard, not a fresh
			// create.
			createBody := `{"schedule":"@every 5m","prompt":"x","notify":true,"notify_platform":"feishu","notify_chat_id":"oc_seed"}`
			cw := postCronCreate(t, srv, createBody)
			if cw.Code != http.StatusOK {
				t.Fatalf("seed create status = %d; body=%s", cw.Code, cw.Body.String())
			}
			var created struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(cw.Body.Bytes(), &created); err != nil {
				t.Fatalf("decode create: %v", err)
			}

			req := httptest.NewRequest(http.MethodPatch, "/api/cron?id="+created.ID, bytes.NewReader([]byte(tc.body)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.mux.ServeHTTP(w, req)
			if w.Code != tc.wantCode {
				t.Fatalf("half-PATCH status = %d, want %d; body=%s", w.Code, tc.wantCode, w.Body.String())
			}

			// Confirm the stored notify halves still match the seed (no
			// partial write). This is the property the caller cares about
			// — the wire-level status is incidental, what matters is that
			// the orphan tuple never reaches disk.
			listReq := httptest.NewRequest(http.MethodGet, "/api/cron", nil)
			listW := httptest.NewRecorder()
			srv.mux.ServeHTTP(listW, listReq)
			var listResp struct {
				Jobs []struct {
					NotifyPlatform string `json:"notify_platform"`
					NotifyChatID   string `json:"notify_chat_id"`
				} `json:"jobs"`
			}
			if err := json.Unmarshal(listW.Body.Bytes(), &listResp); err != nil {
				t.Fatalf("decode list: %v", err)
			}
			if len(listResp.Jobs) != 1 {
				t.Fatalf("jobs = %d, want 1", len(listResp.Jobs))
			}
			if listResp.Jobs[0].NotifyPlatform != "feishu" || listResp.Jobs[0].NotifyChatID != "oc_seed" {
				t.Errorf("seed notify pair drifted after rejected PATCH: platform=%q chat_id=%q",
					listResp.Jobs[0].NotifyPlatform, listResp.Jobs[0].NotifyChatID)
			}
		})
	}
}

// TestCronUpdate_AcceptsBothNotifyPatch pins the positive companion to
// TestCronUpdate_RejectsHalfNotifyPatch: a PATCH that sends BOTH notify
// pointers together must succeed. Without this we'd risk over-tightening
// the half-PATCH gate into a "no notify edits ever" regression.
func TestCronUpdate_AcceptsBothNotifyPatch(t *testing.T) {
	t.Parallel()
	srv := newTestServerWithScheduler(&mockPlatform{})

	createBody := `{"schedule":"@every 5m","prompt":"x","notify":true,"notify_platform":"feishu","notify_chat_id":"oc_seed"}`
	cw := postCronCreate(t, srv, createBody)
	if cw.Code != http.StatusOK {
		t.Fatalf("seed create status = %d; body=%s", cw.Code, cw.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(cw.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	// Both halves cleared together — the legitimate "fall back to default"
	// flow. Must accept.
	patchBody := `{"notify_platform":"","notify_chat_id":""}`
	req := httptest.NewRequest(http.MethodPatch, "/api/cron?id="+created.ID, bytes.NewReader([]byte(patchBody)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("both-cleared PATCH status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}
