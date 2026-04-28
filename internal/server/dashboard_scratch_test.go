package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

// newScratchTestServer wires a newTestServer with a live ScratchPool + handler,
// matching the production registerDashboard path but keeping the limiter
// loose so burst tests don't spuriously trip.
func newScratchTestServer(t *testing.T) *Server {
	t.Helper()
	srv := newTestServer(&mockPlatform{})
	// registerDashboard already created the pool + handler; override the
	// rate limiter with a permissive one so open/close/promote cycles in
	// a single test don't hit the 5/min cap.
	if srv.scratchH == nil {
		t.Fatal("expected scratch handler to be wired by registerDashboard")
	}
	srv.scratchH.openLimit = nil
	return srv
}

func TestScratchOpen_Happy(t *testing.T) {
	srv := newScratchTestServer(t)
	srv.router.InjectSession("feishu:direct:alice:general", session.NewTestProcess())

	body := `{"source_key":"feishu:direct:alice:general","quote":"what does the circuit breaker do?"}`
	req := httptest.NewRequest(http.MethodPost, "/api/scratch/open", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.scratchH.handleOpen(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		ScratchID      string `json:"scratch_id"`
		Key            string `json:"key"`
		AgentID        string `json:"agent_id"`
		QuoteTruncated bool   `json:"quote_truncated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.ScratchID) != 32 {
		t.Errorf("scratch id length = %d", len(resp.ScratchID))
	}
	if !strings.HasPrefix(resp.Key, session.ScratchKeyPrefix) {
		t.Errorf("key missing prefix: %q", resp.Key)
	}
	if resp.QuoteTruncated {
		t.Error("unexpected truncation for short quote")
	}
}

func TestScratchOpen_MissingQuote(t *testing.T) {
	srv := newScratchTestServer(t)
	srv.router.InjectSession("feishu:direct:alice:general", session.NewTestProcess())
	req := httptest.NewRequest(http.MethodPost, "/api/scratch/open",
		strings.NewReader(`{"source_key":"feishu:direct:alice:general"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.scratchH.handleOpen(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestScratchOpen_UnknownSource(t *testing.T) {
	srv := newScratchTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/scratch/open",
		strings.NewReader(`{"source_key":"no:such:chat:general","quote":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.scratchH.handleOpen(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestScratchOpen_SourceIsScratchRefused(t *testing.T) {
	srv := newScratchTestServer(t)
	// Simulate a live scratch source.
	scratchKey := "scratch:aaaaaa:general:general"
	srv.router.InjectSession(scratchKey, session.NewTestProcess())

	body, _ := json.Marshal(map[string]string{"source_key": scratchKey, "quote": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/scratch/open", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.scratchH.handleOpen(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestScratchOpen_InvalidSourceKey(t *testing.T) {
	srv := newScratchTestServer(t)
	body := map[string]string{"source_key": "bad\x00key", "quote": "hi"}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/scratch/open", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.scratchH.handleOpen(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestScratchDelete_InvalidID(t *testing.T) {
	srv := newScratchTestServer(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/scratch/short", nil)
	req.SetPathValue("id", "short")
	w := httptest.NewRecorder()
	srv.scratchH.handleDelete(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestScratchDelete_UnknownReturns204(t *testing.T) {
	// Idempotent delete: unknown but well-formed ID returns 204 so a client
	// retry after sweeper pruning does not surface as an error.
	srv := newScratchTestServer(t)
	req := httptest.NewRequest(http.MethodDelete,
		"/api/scratch/00000000000000000000000000000000", nil)
	req.SetPathValue("id", "00000000000000000000000000000000")
	w := httptest.NewRecorder()
	srv.scratchH.handleDelete(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

func TestScratchDelete_Known(t *testing.T) {
	srv := newScratchTestServer(t)
	srv.router.InjectSession("feishu:direct:alice:general", session.NewTestProcess())
	sc, err := srv.scratchPool.Open(session.OpenOptions{
		SourceKey: "feishu:direct:alice:general",
		AgentID:   "general",
		Quote:     "hello",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/scratch/"+sc.ID, nil)
	req.SetPathValue("id", sc.ID)
	w := httptest.NewRecorder()
	srv.scratchH.handleDelete(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
	if srv.scratchPool.Get(sc.ID) != nil {
		t.Error("scratch still in pool after DELETE")
	}
}

func TestScratchPromote_RenamesSession(t *testing.T) {
	srv := newScratchTestServer(t)
	srv.router.InjectSession("feishu:direct:alice:general", session.NewTestProcess())
	sc, err := srv.scratchPool.Open(session.OpenOptions{
		SourceKey: "feishu:direct:alice:general",
		AgentID:   "general",
		Quote:     "promote me",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// The scratch key must be registered in the router for promote to
	// succeed — InjectSession handles this for a different key; here we
	// inject at sc.Key to simulate the first send having happened.
	srv.router.InjectSession(sc.Key, session.NewTestProcess())

	req := httptest.NewRequest(http.MethodPost, "/api/scratch/"+sc.ID+"/promote", nil)
	req.SetPathValue("id", sc.ID)
	w := httptest.NewRecorder()
	srv.scratchH.handlePromote(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(resp.Key, "feishu:direct:alice:aside-") {
		t.Errorf("promoted key shape unexpected: %q", resp.Key)
	}
	if srv.router.GetSession(resp.Key) == nil {
		t.Error("promoted session not found under new key")
	}
	if srv.router.GetSession(sc.Key) != nil {
		t.Error("old scratch key still present after promote")
	}
	if srv.scratchPool.Get(sc.ID) != nil {
		t.Error("pool should have detached scratch after promote")
	}
}

func TestScratchPromote_UnknownID(t *testing.T) {
	srv := newScratchTestServer(t)
	id := "00000000000000000000000000000000"
	req := httptest.NewRequest(http.MethodPost, "/api/scratch/"+id+"/promote", nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	srv.scratchH.handlePromote(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestScratchListFilteredFromSessions(t *testing.T) {
	// handleList must hide scratch keys from the sidebar payload.
	srv := newScratchTestServer(t)
	srv.router.InjectSession("feishu:direct:alice:general", session.NewTestProcess())
	// 32-char scratch id (lowercase hex) matches the newScratchID shape.
	scratchKey := "scratch:cccccccccccccccccccccccccccccccc:general:general"
	srv.router.InjectSession(scratchKey, session.NewTestProcess())

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.sessionH.handleList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "scratch:") {
		t.Errorf("sidebar payload should not include scratch keys: %s", w.Body.String())
	}
}

// Sanity check that the sweeper is started as part of registerDashboard
// and actually drops an idle scratch. Backdates lastUsed then triggers
// sweep manually via exposed test seam.
func TestScratchSweeperRegistered(t *testing.T) {
	srv := newScratchTestServer(t)
	srv.router.InjectSession("feishu:direct:alice:general", session.NewTestProcess())
	sc, _ := srv.scratchPool.Open(session.OpenOptions{
		SourceKey: "feishu:direct:alice:general",
		AgentID:   "general",
		Quote:     "fade me",
	})
	// Force-expire by back-dating; we don't wait for the real tick here.
	srv.scratchPool.ForceExpireForTest(sc.ID, time.Now().Add(-2*session.DefaultScratchTTL))
	srv.scratchPool.SweepForTest(time.Now())
	if srv.scratchPool.Get(sc.ID) != nil {
		t.Error("sweeper should have dropped expired scratch")
	}
}
