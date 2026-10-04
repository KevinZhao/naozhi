package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeHistorySession puts a transcript under claudeDir/projects for a
// workspace that exists on disk, so a history scan lists sessionID.
func writeHistorySession(t *testing.T, claudeDir, workspace, sessionID string) {
	t.Helper()
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	projDir := filepath.Join(claudeDir, "projects", strings.ReplaceAll(workspace, "/", "-"))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","message":{"role":"user","content":"hi"}}` + "\n"
	if err := os.WriteFile(filepath.Join(projDir, sessionID+".jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	histSessA = "11111111-1111-4111-8111-111111111111"
	histSessB = "22222222-2222-4222-8222-222222222222"
)

// newHistoryTestHandlers serves one scanned history session.
func newHistoryTestHandlers(t *testing.T) (h *Handlers, claudeDir, workspace string) {
	t.Helper()
	h = newETagTestHandlers(t, newListRouter("feishu:direct:a:general"), noNodeAccessor{})
	claudeDir, workspace = t.TempDir(), filepath.Join(t.TempDir(), "wsproj")
	h.deps.ClaudeDir = claudeDir
	writeHistorySession(t, claudeDir, workspace, histSessA)
	return h, claudeDir, workspace
}

func doHistory(h *Handlers, ifNoneMatch string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/history", nil)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	h.HandleHistory(rec, req)
	return rec
}

func decodeHistory(t *testing.T, rec *httptest.ResponseRecorder) historyListResp {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp historyListResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return resp
}

// The tag names the list by content: a rescan that finds the same sessions
// keeps it, so the dashboard does not refetch, and a new session moves it.
func TestHistoryTag_FollowsContentAcrossRescans(t *testing.T) {
	h, claudeDir, workspace := newHistoryTestHandlers(t)
	list, tag := h.historyWithTag()
	if len(list) != 1 || tag == "" {
		t.Fatalf("first scan: %d sessions, tag %q; want one session under a tag", len(list), tag)
	}
	h.InvalidateHistoryCache()
	if _, again := h.historyWithTag(); again != tag {
		t.Errorf("identical rescan moved the tag: %q -> %q", tag, again)
	}
	if h.IsHistoryCacheTimeZeroForTest() {
		t.Fatal("historyWithTag did not rescan after InvalidateHistoryCache")
	}
	writeHistorySession(t, claudeDir, workspace, histSessB)
	h.InvalidateHistoryCache()
	if list, moved := h.historyWithTag(); len(list) != 2 || moved == tag || moved == "" {
		t.Errorf("rescan with a new session: %d sessions, tag %q (was %q); want 2 under a new tag", len(list), moved, tag)
	}
}

// /api/sessions carries the tag in place of the list, and the history
// endpoint serves the list that tag names.
func TestHandleHistory_ServesTheListStatsNames(t *testing.T) {
	h, _, _ := newHistoryTestHandlers(t)
	rec := doList(h, "")
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["history_sessions"]; ok {
		t.Error("/api/sessions still embeds history_sessions")
	}
	var stats struct {
		HistoryTag string `json:"history_tag"`
	}
	if err := json.Unmarshal(body["stats"], &stats); err != nil {
		t.Fatal(err)
	}
	if stats.HistoryTag == "" {
		t.Fatal("/api/sessions stats.history_tag is empty with one history session")
	}

	hist := doHistory(h, "")
	resp := decodeHistory(t, hist)
	if resp.HistoryTag != stats.HistoryTag {
		t.Errorf("history_tag = %q, want the stats tag %q", resp.HistoryTag, stats.HistoryTag)
	}
	if len(resp.HistorySessions) != 1 || resp.HistorySessions[0].SessionID != histSessA {
		t.Errorf("history_sessions = %+v, want the one scanned session", resp.HistorySessions)
	}
	if got, want := hist.Header().Get("ETag"), `"h`+stats.HistoryTag+`"`; got != want {
		t.Errorf("ETag = %q, want %q", got, want)
	}
}

func TestHandleHistory_IfNoneMatch(t *testing.T) {
	h, _, _ := newHistoryTestHandlers(t)
	etag := doHistory(h, "").Header().Get("ETag")
	for _, inm := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
		rec := doHistory(h, inm)
		if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
			t.Errorf("If-None-Match %s: status %d, body %d bytes; want a bodyless 304", inm, rec.Code, rec.Body.Len())
			continue
		}
		if rec.Header().Get("ETag") != etag || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("If-None-Match %s: 304 headers ETag %q, Cache-Control %q", inm, rec.Header().Get("ETag"), rec.Header().Get("Cache-Control"))
		}
	}
	for _, inm := range []string{`"hother"`, strings.Trim(etag, `"`)} {
		if rec := doHistory(h, inm); rec.Code != http.StatusOK {
			t.Errorf("If-None-Match %s: status %d, want 200", inm, rec.Code)
		}
	}
}

// A scan that finds no history serves [] (never null) under no tag, and
// /api/sessions leaves history_tag out.
func TestHandleHistory_EmptyHistory(t *testing.T) {
	h := newETagTestHandlers(t, newListRouter("feishu:direct:a:general"), noNodeAccessor{})
	h.deps.ClaudeDir = t.TempDir()
	rec := doHistory(h, "")
	if got := strings.TrimSpace(rec.Body.String()); got != `{"history_sessions":[]}` {
		t.Errorf("body = %s, want {\"history_sessions\":[]}", got)
	}
	if etag := rec.Header().Get("ETag"); etag != "" {
		t.Errorf("ETag = %q for an empty history, want none", etag)
	}
	if list := doList(h, ""); strings.Contains(list.Body.String(), "history_tag") {
		t.Errorf("/api/sessions carries history_tag with no history: %s", list.Body.String())
	}
}
