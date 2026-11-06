package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
)

func user(t int64) clievent.EventEntry { return clievent.EventEntry{Time: t, Type: clievent.KindUser} }
func reply(t int64, text string) clievent.EventEntry {
	return clievent.EventEntry{Time: t, Type: clievent.KindText, Detail: text, UUID: "u" + text}
}

func TestLastTurnReplies(t *testing.T) {
	entries := []clievent.EventEntry{
		reply(1, "before any turn"),
		user(2), reply(3, "first"),
		user(4), {Time: 5, Type: clievent.KindToolUse, Detail: "tool text"}, reply(6, "second"),
		user(7), {Time: 8, Type: clievent.KindText, Summary: "summary only"}, reply(9, "third"),
	}
	texts := func(rs []replyText) string {
		var out []string
		for _, r := range rs {
			out = append(out, r.text)
		}
		return strings.Join(out, "|")
	}
	cases := []struct {
		name     string
		turns    int
		maxBytes int
		want     string
	}{
		{"last turn, newest first, summary fallback", 1, 0, "third|summary only"},
		{"tool events are not replies", 2, 0, "third|summary only|second"},
		{"older turns are not taken", 3, 0, "third|summary only|second|first"},
		{"a short log is taken whole", 9, 0, "third|summary only|second|first|before any turn"},
		{"byte cap keeps the newest", 3, len("third"), "third"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := texts(lastTurnReplies(entries, c.turns, c.maxBytes)); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestSnippetAround(t *testing.T) {
	long := strings.Repeat("前", 100) + "g6e" + strings.Repeat("后", 100) + "。"
	cases := []struct {
		name, text, q, want string
	}{
		{"the sentence holding it", "先说结论。g6e 适合推理，p5 适合训练。其他略", "G6E", "g6e 适合推理，p5 适合训练。"},
		{"a dotted name is one sentence", "Done. g6e.12xlarge 在 us-east-1 最便宜. Next", "12xl", "g6e.12xlarge 在 us-east-1 最便宜."},
		{"markdown is stripped", "列表：\n- **g6e 适合**中小模型推理\n- p5", "中小", "g6e 适合中小模型推理"},
		{"a long sentence is cut around the match", long, "g6e", "…" + strings.Repeat("前", 38) + "g6e" + strings.Repeat("后", 39) + "…"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rs := []rune(c.text)
			i := foldIndex(rs, foldRunes(c.q))
			if i < 0 {
				t.Fatalf("%q not found", c.q)
			}
			if got := snippetAround(rs, i, len([]rune(c.q))); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// writeTranscript puts a transcript of lines under claudeDir/projects for a
// workspace that exists, so a history scan lists sessionID.
func writeTranscript(t *testing.T, claudeDir, workspace, sessionID string, lines ...string) {
	t.Helper()
	writeHistorySession(t, claudeDir, workspace, sessionID)
	path := filepath.Join(claudeDir, "projects", strings.ReplaceAll(workspace, "/", "-"), sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func asstLine(ts, text string) string {
	return `{"type":"assistant","timestamp":"` + ts + `","message":{"role":"assistant","content":[{"type":"text","text":"` + text + `"}]}}`
}

func userLine(ts, text string) string {
	return `{"type":"user","timestamp":"` + ts + `","message":{"role":"user","content":"` + text + `"}}`
}

// GET /api/sessions/history?q= finds the query in the recent replies of the
// sidebar's sessions and of the history sessions, and names the reply.
func TestHandleSearch(t *testing.T) {
	r := sessionpkg.NewRouter(sessionpkg.RouterConfig{MaxProcs: 4, StorePath: filepath.Join(t.TempDir(), "sessions.json")})
	t.Cleanup(r.Shutdown)
	live := sessionpkg.NewTestProcess()
	for _, e := range []clievent.EventEntry{
		user(1000), reply(1100, "很久以前提过 g6e。"),
		user(2000), reply(2100, "a"), user(3000), reply(3100, "b"),
		user(4000), reply(4100, "结论：g6e 适合推理。"),
	} {
		live.EventLog.Append(e)
	}
	r.InjectSession("dashboard:direct:a:general", live)
	cron := sessionpkg.NewTestProcess()
	cron.EventLog.Append(reply(5000, "cron 也提过 g6e"))
	r.InjectSession("cron:job1", cron)

	h := New(Deps{Router: realRouter{r}})
	claudeDir, workspace := t.TempDir(), filepath.Join(t.TempDir(), "wsproj")
	h.deps.ClaudeDir = claudeDir
	writeTranscript(t, claudeDir, workspace, histSessA,
		userLine("2026-01-01T00:00:00Z", "查一下"),
		asstLine("2026-01-01T00:01:00Z", "g6e.12xlarge 的 SPS 最高。"))
	writeTranscript(t, claudeDir, workspace, histSessB,
		userLine("2026-01-01T00:00:00Z", "别的"),
		asstLine("2026-01-01T00:01:00Z", "没有提到"))

	search := func(t *testing.T, q string) searchResp {
		t.Helper()
		rec := httptest.NewRecorder()
		h.HandleHistory(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/history?q="+url.QueryEscape(q), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		var resp searchResp
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Hits == nil {
			t.Fatalf("hits is null: %s", rec.Body)
		}
		return resp
	}

	got := search(t, "G6E")
	if len(got.Hits) != 2 || got.Partial {
		t.Fatalf("hits = %+v partial=%v, want the live and the history session", got.Hits, got.Partial)
	}
	liveHit, histHit := got.Hits[0], got.Hits[1]
	if liveHit.Key == "" {
		liveHit, histHit = histHit, liveHit
	}
	if liveHit.Key != "dashboard:direct:a:general" || liveHit.Time != 4100 || liveHit.UUID != "u结论：g6e 适合推理。" || liveHit.Snippet != "结论：g6e 适合推理。" {
		t.Errorf("live hit = %+v, want the last turn's reply", liveHit)
	}
	wantTime := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC).UnixMilli()
	if histHit.SessionID != histSessA || histHit.Key != "" || histHit.Time != wantTime || histHit.UUID == "" || histHit.Snippet != "g6e.12xlarge 的 SPS 最高。" {
		t.Errorf("history hit = %+v, want session A's reply at %d", histHit, wantTime)
	}

	// The history replies are cached at the transcript's mtime: a query
	// reads no transcript twice while the list is unchanged.
	if err := os.Remove(filepath.Join(claudeDir, "projects", strings.ReplaceAll(workspace, "/", "-"), histSessA+".jsonl")); err != nil {
		t.Fatal(err)
	}
	if got := search(t, "sps"); len(got.Hits) != 1 || got.Hits[0].SessionID != histSessA {
		t.Errorf("cached search hits = %+v, want session A from the cache", got.Hits)
	}

	// "很久以前" is only in a reply older than the last searchTurns turns.
	for _, q := range []string{"", " g ", "没有这个词", "很久以前"} {
		if got := search(t, q); len(got.Hits) != 0 {
			t.Errorf("q=%q hits = %+v, want none", q, got.Hits)
		}
	}
}

// newSearchHandlers serves the history sessions of a fresh Claude dir; the
// router holds none.
func newSearchHandlers(t *testing.T) (h *Handlers, claudeDir, workspace string) {
	t.Helper()
	r := sessionpkg.NewRouter(sessionpkg.RouterConfig{MaxProcs: 4, StorePath: filepath.Join(t.TempDir(), "sessions.json")})
	t.Cleanup(r.Shutdown)
	h = New(Deps{Router: realRouter{r}})
	claudeDir, workspace = t.TempDir(), filepath.Join(t.TempDir(), "wsproj")
	h.deps.ClaudeDir = claudeDir
	return h, claudeDir, workspace
}

// Queries that overlap while the cache is cold read each transcript once:
// the later one waits for the reading one and searches what it cached.
func TestSearchHistoryReadsOnceAcrossConcurrentQueries(t *testing.T) {
	h, claudeDir, workspace := newSearchHandlers(t)
	writeTranscript(t, claudeDir, workspace, histSessA, userLine("2026-01-01T00:00:00Z", "q"), asstLine("2026-01-01T00:01:00Z", "g6e 最便宜"))
	writeTranscript(t, claudeDir, workspace, histSessB, userLine("2026-01-01T00:00:00Z", "q"), asstLine("2026-01-01T00:01:00Z", "g6e 最快"))

	var mu sync.Mutex
	reads := 0
	entered, release := make(chan struct{}), make(chan struct{})
	orig := loadHistoryTail
	t.Cleanup(func() { loadHistoryTail = orig })
	loadHistoryTail = func(ctx context.Context, claudeDir, sessionID, cwd string, limit int) ([]clievent.EventEntry, error) {
		mu.Lock()
		reads++
		first := reads == 1
		mu.Unlock()
		if first {
			close(entered)
			<-release
		}
		return orig(ctx, claudeDir, sessionID, cwd, limit)
	}

	type result struct {
		hits    int
		partial bool
	}
	search := func(out chan<- result) {
		ctx, cancel := context.WithTimeout(context.Background(), searchReadBudget)
		defer cancel()
		hits, partial := h.searchHistory(ctx, foldRunes("g6e"))
		out <- result{len(hits), partial}
	}
	first, second := make(chan result, 1), make(chan result, 1)
	go search(first)
	<-entered
	go search(second)
	// Time for the second query to read on its own, as it would unserialized.
	time.Sleep(200 * time.Millisecond)
	close(release)

	for _, ch := range []chan result{first, second} {
		if r := <-ch; r.hits != 2 || r.partial {
			t.Errorf("query = %+v, want both sessions, not partial", r)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if reads != 2 {
		t.Errorf("transcript reads = %d, want 2 (one per session)", reads)
	}
}

// A query whose budget ends while another one reads answers from the cache
// alone, marked partial.
func TestSearchHistoryWaiterAnswersFromCache(t *testing.T) {
	h, claudeDir, workspace := newSearchHandlers(t)
	writeTranscript(t, claudeDir, workspace, histSessA, userLine("2026-01-01T00:00:00Z", "q"), asstLine("2026-01-01T00:01:00Z", "g6e 最便宜"))

	search := func(budget time.Duration) (int, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		hits, partial := h.searchHistory(ctx, foldRunes("g6e"))
		return len(hits), partial
	}
	slot := h.replies.readSlot()
	slot <- struct{}{} // another query is reading
	if n, partial := search(50 * time.Millisecond); n != 0 || !partial {
		t.Errorf("waiting query = %d hits partial=%v, want none and partial", n, partial)
	}
	<-slot
	if n, partial := search(searchReadBudget); n != 1 || partial {
		t.Errorf("query after the reader = %d hits partial=%v, want the session", n, partial)
	}
}
