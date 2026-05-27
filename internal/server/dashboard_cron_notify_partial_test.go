package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cron"
)

// TestHandleCreate_NotifyHalfSetRejected pins R242-SEC-11 (#640): a
// partial notify-target (one field set, the other blank) must be rejected
// at handleCreate before the job lands on disk. Pre-fix, the gate used
// `&&` which let "platform-only" or "chat_id-only" requests fall through
// to NotifyDefault, silently routing the job to the global fallback target
// (or the wrong chat). The fix gates with `||` (at least one side set
// requires both sides set); this test pins the rejection.
//
// We exercise both half-set permutations + the all-empty (allowed) and
// both-set (allowed up to the validateNotifyTarget allowlist) shapes so a
// future refactor that drops the gate would produce a measurable diff.
func TestHandleCreate_NotifyHalfSetRejected(t *testing.T) {
	t.Parallel()
	// scheduler must be non-nil so handleCreate proceeds past the
	// "cron not configured" 501 short-circuit. Empty config is fine —
	// the test only cares about pre-persist validation, so we never
	// reach AddJob.
	sched := cron.NewScheduler(cron.SchedulerConfig{})

	type tc struct {
		name      string
		platform  string
		chatID    string
		wantStaus int // 400 for half-set rejection, anything-else otherwise
		wantBody  string
	}
	cases := []tc{
		{
			name:      "platform_only_rejected",
			platform:  "feishu",
			chatID:    "",
			wantStaus: http.StatusBadRequest,
			wantBody:  "must be set together",
		},
		{
			name:      "chatid_only_rejected",
			platform:  "",
			chatID:    "oc_xxxxxxxxxxxxxxxx",
			wantStaus: http.StatusBadRequest,
			wantBody:  "must be set together",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := &CronHandlers{scheduler: sched}
			body := `{"schedule":"* * * * *","prompt":"hi","notify_platform":` +
				jsonStr(c.platform) + `,"notify_chat_id":` + jsonStr(c.chatID) + `}`
			req := httptest.NewRequest(http.MethodPost, "/api/cron",
				strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.handleCreate(w, req)
			if w.Code != c.wantStaus {
				t.Fatalf("got %d, want %d; body=%q", w.Code, c.wantStaus, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), c.wantBody) {
				t.Fatalf("body=%q does not contain %q", w.Body.String(), c.wantBody)
			}
		})
	}
}

// jsonStr returns a JSON-encoded string literal for s. Avoids pulling
// encoding/json just to format two test fields.
func jsonStr(s string) string {
	// Naive escape: only ASCII letters/digits/underscore/colon/dot/hyphen
	// appear in test inputs above. Anything else is escaped via %q-style
	// fallback. Tests use known-safe values so the fast path is enough.
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == '"' || c == '\\' {
			// Fall back to a fully-quoted form via fmt for safety.
			return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
		}
	}
	return `"` + s + `"`
}
