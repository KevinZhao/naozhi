package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHTTPErrPersistFailed_JSONEnvelope pins R20260527-ARCH-2 (#1274):
// httpErrPersistFailed used to write a text/plain http.Error body; the
// five cron write handlers (create/delete/pause/resume/update) all
// surface cronpkg.ErrPersistFailed through this helper, so dashboard.js
// would have to branch on Content-Type to read the message. The helper
// now writes the unified errResp envelope so the entire cron persist-
// failure surface ships JSON {error, code} consistently.
//
// Asserts:
//   - status code 500 (preserved from the legacy shape);
//   - Content-Type starts with application/json (errResp contract);
//   - body parses as JSON with the operator-facing message under "error"
//     and the machine-readable token "persist_failed" under "code".
//
// Regression for: a future "fix" reverting the helper to http.Error or
// dropping the code field would silently re-introduce the dual-format
// branching cost on the front-end.
func TestHTTPErrPersistFailed_JSONEnvelope(t *testing.T) {
	t.Parallel()

	for _, op := range []string{"created", "deleted", "paused", "resumed", "updated"} {
		op := op
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			w := httptest.NewRecorder()
			httpErrPersistFailed(w, op)

			if got := w.Code; got != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d (legacy shape preserved)", got, http.StatusInternalServerError)
			}
			ct := w.Result().Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("Content-Type = %q, want application/json prefix — helper must write the errResp envelope, not text/plain", ct)
			}

			var body struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v; raw=%q", err, w.Body.String())
			}
			if body.Code != "persist_failed" {
				t.Errorf("code = %q, want %q (stable machine-readable token)", body.Code, "persist_failed")
			}
			if !strings.Contains(body.Error, op) {
				t.Errorf("error = %q, want substring %q (verb preserved so log scrapers stay green)", body.Error, op)
			}
			if !strings.Contains(body.Error, "not persisted") {
				t.Errorf("error = %q, want substring %q (operator-facing wording stable)", body.Error, "not persisted")
			}
		})
	}
}
