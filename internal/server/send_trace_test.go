package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/ctxutil"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// turnStart is the first "turn: start" record in l, or nil.
func turnStart(l *lockedBuf) map[string]any {
	for _, line := range strings.Split(l.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "turn: start" {
			return m
		}
	}
	return nil
}

// An HTTP send's X-Request-ID is the trace of the turn it starts, though
// the turn runs on the engine's ctx rather than the request's.
func TestHandleSend_RequestIDIsTurnTrace(t *testing.T) {
	buf := &lockedBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(ctxutil.NewHandler(slog.NewJSONHandler(buf, nil))))
	t.Cleanup(func() { slog.SetDefault(prev) })

	hub, _ := newTestHub(t, "")
	t.Cleanup(hub.Shutdown)
	h := &SendHandler{engine: hub.engine, uploadStore: newUploadStore()}
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(`{"key":"test:d:u:general","text":"hello"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer tok")
	r.Header.Set(traceIDHeader, "req-abc")
	w := httptest.NewRecorder()
	withTraceID(http.HandlerFunc(h.handleSend)).ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}

	var m map[string]any
	testhelper.Eventually(t, func() bool { m = turnStart(buf); return m != nil }, 3*time.Second, "no turn: start line logged")
	if m["trace_id"] != "req-abc" || m["session_key"] != "test:d:u:general" {
		t.Fatalf("turn: start = %v, want trace_id req-abc", m)
	}
}
