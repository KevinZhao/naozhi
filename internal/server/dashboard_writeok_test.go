package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWriteOK locks the wire format of the pre-marshaled `{"status":"ok"}`
// body so refactors cannot quietly drop the trailing newline (which breaks
// bash pipelines expecting NDJSON framing) or flip the key name.
// R64-PERF-M4 regression.
func TestWriteOK(t *testing.T) {
	w := httptest.NewRecorder()
	writeOK(w)

	if got, want := w.Code, 200; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
	if got, want := w.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got, want := w.Header().Get("X-Content-Type-Options"), "nosniff"; got != want {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, want)
	}
	if got, want := w.Header().Get("Cache-Control"), "no-store"; got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
	body := w.Body.String()
	if !strings.HasSuffix(body, "\n") {
		t.Errorf("body missing trailing newline: %q", body)
	}
	if trimmed := strings.TrimSuffix(body, "\n"); trimmed != `{"status":"ok"}` {
		t.Errorf("body = %q, want %q", trimmed, `{"status":"ok"}`)
	}
}

// TestWriteJSON_SetsNoStoreCache locks the R58-PERF-001 contract that
// authenticated dashboard JSON responses carry Cache-Control: no-store, so no
// intermediate proxy or browser bfcache retains last_prompt / PID / cost state
// that would leak to the next user on the same cache. Exercised via writeJSON
// (a small opaque payload is enough — we only care about headers).
func TestWriteJSON_SetsNoStoreCache(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, map[string]string{"hello": "world"})

	if got, want := w.Header().Get("Cache-Control"), "no-store"; got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
	if got, want := w.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got, want := w.Header().Get("X-Content-Type-Options"), "nosniff"; got != want {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, want)
	}
}

// TestWriteJSONStatus_SetsNoStoreCache mirrors TestWriteJSON_SetsNoStoreCache
// for the non-200 path; error responses can still contain auth-sensitive
// context (e.g. session keys in validation failures) and must not be cached.
func TestWriteJSONStatus_SetsNoStoreCache(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSONStatus(w, 400, map[string]string{"error": "bad request"})

	if got, want := w.Code, 400; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
	if got, want := w.Header().Get("Cache-Control"), "no-store"; got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
}
