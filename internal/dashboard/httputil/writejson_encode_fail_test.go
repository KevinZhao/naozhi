package httputil

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWriteJSON_EncodeFailureReturns500 pins the failure mode behind the
// dashboard "panel shows nothing" bugs: when the value cannot be encoded
// (here an invalid json.RawMessage) WriteJSON used to log at Debug and return
// with the implicit 200 + empty body. The client must instead see a 500 with a
// JSON error envelope so the failure is visible and retryable.
func TestWriteJSON_EncodeFailureReturns500(t *testing.T) {
	w := httptest.NewRecorder()
	WriteJSON(w, map[string]any{"events": []json.RawMessage{json.RawMessage(`{"a":`)}})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%q)", w.Code, w.Body.String())
	}
	if !json.Valid(w.Body.Bytes()) || w.Body.Len() == 0 {
		t.Fatalf("body must be a JSON error envelope, got %q", w.Body.String())
	}
	var env map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || env["error"] == "" {
		t.Fatalf("expected {\"error\":...}, got %q", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
}
