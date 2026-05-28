package cron

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// TestCronWriteHandlers_IDShapeGate locks R250-SEC-1: the 5 cron write
// handlers (Delete, Pause, Resume, Trigger, Update) must reject ids whose
// bytes fall outside lowercase-hex *before* the value reaches the
// scheduler / slog. Without this gate, a stolen dashboard token could
// embed CR/LF/control bytes in the id; the scheduler's ErrJobNotFound
// path would then echo the raw bytes into the operator log via
// slog.Warn / "id %q", forging fake log records.
//
// We assert two invariants for each write handler:
//  1. HTTP status is 400 (not 404 / 401 / 500).
//  2. No slog record is emitted for the bad id (drop happens at the
//     edge, before any logging path that would carry attacker bytes).
func TestCronWriteHandlers_IDShapeGate(t *testing.T) {
	// Not t.Parallel: this test swaps slog.Default (process-global) to
	// capture log output. Running in parallel with peer tests that emit
	// slog records (e.g. NewWithOptions → buildServer's allowed_root
	// Warn at server.go:570) races on the captured bytes.Buffer because
	// the swapped logger is still installed when those peers fire.
	// The whole suite is fast so serialising costs nothing.

	// Bad ids: every value must be ≤ maxCronIDLenDashboard so the length
	// gate doesn't short-circuit the test — we want to assert the *shape*
	// gate kicks in (post length, pre scheduler).
	bad := []struct {
		name string
		id   string
	}{
		{"newline_log_inject", "abc\nfake-log-line"},
		{"carriage_return", "abc\rdef"},
		{"null_byte", "abc\x00def"},
		{"escape", "abc\x1bdef"},
		{"uppercase_hex", "ABCDEF"},
		{"non_hex_letter", "deadbeefz"},
		{"space", "abc def"},
		{"slash", "abc/def"},
	}

	// Sanity guard: confirm IsValidID actually rejects each fixture so the
	// test would fail loudly if the production helper accidentally became
	// permissive in the future.
	for _, tc := range bad {
		if cronpkg.IsValidID(tc.id) {
			t.Fatalf("test fixture %q is unexpectedly considered valid by cronpkg.IsValidID; tighten test data", tc.id)
		}
	}

	type handlerCase struct {
		name        string
		method      string
		path        string
		body        func(id string) string // empty means use query string
		queryKey    string                 // when body is empty
		invoke      func(h *Handlers, w http.ResponseWriter, r *http.Request)
		contentType string
	}

	handlers := []handlerCase{
		{
			name:     "delete_query_id",
			method:   http.MethodDelete,
			path:     "/api/cron",
			queryKey: "id",
			invoke:   func(h *Handlers, w http.ResponseWriter, r *http.Request) { h.HandleDelete(w, r) },
		},
		{
			name:        "pause_body_id",
			method:      http.MethodPost,
			path:        "/api/cron/pause",
			body:        func(id string) string { return `{"id":` + jsonQuote(id) + `}` },
			contentType: "application/json",
			invoke:      func(h *Handlers, w http.ResponseWriter, r *http.Request) { h.HandlePause(w, r) },
		},
		{
			name:        "resume_body_id",
			method:      http.MethodPost,
			path:        "/api/cron/resume",
			body:        func(id string) string { return `{"id":` + jsonQuote(id) + `}` },
			contentType: "application/json",
			invoke:      func(h *Handlers, w http.ResponseWriter, r *http.Request) { h.HandleResume(w, r) },
		},
		{
			name:        "trigger_body_id",
			method:      http.MethodPost,
			path:        "/api/cron/trigger",
			body:        func(id string) string { return `{"id":` + jsonQuote(id) + `}` },
			contentType: "application/json",
			invoke:      func(h *Handlers, w http.ResponseWriter, r *http.Request) { h.HandleTrigger(w, r) },
		},
		{
			name:     "update_query_id",
			method:   http.MethodPatch,
			path:     "/api/cron",
			queryKey: "id",
			invoke:   func(h *Handlers, w http.ResponseWriter, r *http.Request) { h.HandleUpdate(w, r) },
		},
	}

	// Sequential subtests on purpose: we swap slog.Default (process-global)
	// to capture log output, which is not safe under t.Parallel. The whole
	// suite is fast so serialising costs nothing.
	for _, hc := range handlers {
		hc := hc
		for _, tc := range bad {
			tc := tc
			t.Run(hc.name+"/"+tc.name, func(t *testing.T) {
				// Capture slog output so we can assert no record is emitted
				// for the rejected id.
				var logBuf bytes.Buffer
				logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

				h := &Handlers{scheduler: cronpkg.NewScheduler(cronpkg.SchedulerConfig{})}

				var req *http.Request
				if hc.body != nil {
					req = httptest.NewRequest(hc.method, hc.path, strings.NewReader(hc.body(tc.id)))
					req.Header.Set("Content-Type", hc.contentType)
				} else {
					// Query-id path — use url.Values to ensure proper
					// encoding of control bytes.
					q := make([]byte, 0, 64)
					q = append(q, hc.queryKey...)
					q = append(q, '=')
					for i := 0; i < len(tc.id); i++ {
						c := tc.id[i]
						// Percent-encode non-alphanumeric bytes so the URL
						// parser delivers the exact bytes to the handler.
						if (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
							q = append(q, c)
							continue
						}
						q = append(q, '%')
						const hex = "0123456789abcdef"
						q = append(q, hex[c>>4], hex[c&0x0f])
					}
					req = httptest.NewRequest(hc.method, hc.path+"?"+string(q), nil)
				}

				// Route the per-test logger via context-bound default —
				// we swap the package default for the duration of the
				// call and restore it on exit. Sequential within the
				// subtest goroutine so no race with parallel siblings.
				prev := slog.Default()
				slog.SetDefault(logger)
				defer slog.SetDefault(prev)

				w := httptest.NewRecorder()
				hc.invoke(h, w, req.WithContext(context.Background()))

				if w.Code != http.StatusBadRequest {
					t.Fatalf("expected 400 for invalid id %q, got %d body=%q", tc.id, w.Code, w.Body.String())
				}

				// The shape gate must drop the request *before* any
				// slog.Info / slog.Warn / slog.Debug call carries the
				// attacker-supplied id. Allow nothing in the buffer.
				if got := logBuf.String(); got != "" {
					t.Fatalf("expected no log output for invalid id (log-injection guard), got: %q", got)
				}
			})
		}
	}
}

// jsonQuote escapes a string for embedding inside a JSON literal in a test
// fixture. We can't pull in encoding/json just to render a tiny envelope —
// inlining keeps the test self-contained.
func jsonQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 {
				const hex = "0123456789abcdef"
				b.WriteString(`\u00`)
				b.WriteByte(hex[c>>4])
				b.WriteByte(hex[c&0x0f])
				continue
			}
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
