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
	body := w.Body.String()
	if !strings.HasSuffix(body, "\n") {
		t.Errorf("body missing trailing newline: %q", body)
	}
	if trimmed := strings.TrimSuffix(body, "\n"); trimmed != `{"status":"ok"}` {
		t.Errorf("body = %q, want %q", trimmed, `{"status":"ok"}`)
	}
}
