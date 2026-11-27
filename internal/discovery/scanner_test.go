package discovery

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
)

// newPromptEntry builds a *promptCacheEntry with gen initialised to v.
// The map stores *promptCacheEntry (#2322), so tests construct entries this way
// instead of fabricating a standalone *atomic.Uint64.
func newPromptEntry(mtime int64, prompt string, gen uint64) *promptCacheEntry {
	e := &promptCacheEntry{mtime: mtime, prompt: prompt}
	e.gen.Store(gen)
	return e
}

// newSummaryEntry builds a *summaryCacheEntry with gen initialised to v. The
// map stores *summaryCacheEntry (#2330) with an inline atomic gen.
func newSummaryEntry(mtime int64, idx sessionsIndex, gen uint64) *summaryCacheEntry {
	e := &summaryCacheEntry{mtime: mtime, index: idx}
	e.gen.Store(gen)
	return e
}

// ---------------------------------------------------------------------------
// helpers shared across scanner tests
// ---------------------------------------------------------------------------

// resetCaches clears the process-wide DefaultScanner's caches so tests
// that rely on package-level Scan/LookupSummaries wrappers start from a
// clean slate. For tests that want parallelism, prefer NewScanner() +
// the *Scanner methods so each subtest has isolated caches.
func resetCaches(t *testing.T) {
	t.Helper()
	sc := DefaultScanner()
	sc.promptCache.Lock()
	sc.promptCache.entries = make(map[string]*promptCacheEntry)
	sc.promptCache.generation.Store(0)
	sc.promptCache.Unlock()

	sc.summaryCache.Lock()
	sc.summaryCache.entries = make(map[string]*summaryCacheEntry)
	sc.summaryCache.generation.Store(0)
	sc.summaryCache.Unlock()

	t.Cleanup(func() {
		sc := DefaultScanner()
		sc.promptCache.Lock()
		sc.promptCache.entries = make(map[string]*promptCacheEntry)
		sc.promptCache.generation.Store(0)
		sc.promptCache.Unlock()

		sc.summaryCache.Lock()
		sc.summaryCache.entries = make(map[string]*summaryCacheEntry)
		sc.summaryCache.generation.Store(0)
		sc.summaryCache.Unlock()
	})
}

// makeJSONLWithUserPrompts creates a JSONL file with user messages.
func makeJSONLWithUserPrompts(t *testing.T, path string, prompts []string) {
	t.Helper()
	var lines []string
	for i, p := range prompts {
		ts := fmt.Sprintf("2026-01-01T%02d:00:00Z", i%24)
		msg, _ := json.Marshal(struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{Role: "user", Content: p})
		lines = append(lines, fmt.Sprintf(`{"type":"user","timestamp":%q,"message":%s}`, ts, string(msg)))
	}
	writeJSONLFile(t, path, lines)
}

// writeSessionsIndex writes a sessions-index.json to projDir.
func writeSessionsIndex(t *testing.T, projDir string, idx sessionsIndex) {
	t.Helper()
	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "sessions-index.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// makeSessionFile writes ~/.claude/sessions/{pid}.json.
func makeSessionFile(t *testing.T, sessDir string, sf sessionFile) {
	t.Helper()
	data, err := json.Marshal(sf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, fmt.Sprintf("%d.json", sf.PID)), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// projectFor makes cwd's project dir under a fresh ~/.claude and, when prompt
// is not empty, a <sessionID>.jsonl in it holding that one user prompt.
func projectFor(t *testing.T, cwd, sessionID, prompt string) (claudeDir, projDir, jsonlPath string) {
	t.Helper()
	claudeDir = makeClaudeDir(t)
	projDir = filepath.Join(claudeDir, "projects", claudefs.ProjectSlug(cwd))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonlPath = filepath.Join(projDir, sessionID+".jsonl")
	if prompt != "" {
		makeJSONLWithUserPrompts(t, jsonlPath, []string{prompt})
	}
	return claudeDir, projDir, jsonlPath
}

// ---------------------------------------------------------------------------
// LookupSummaries
// ---------------------------------------------------------------------------

// TestLookupSummaries: each session's summary from its project's index, the
// same on a second (cached) call; a session the index does not list, or whose
// project has no index, gets no entry; empty inputs give nil.
func TestLookupSummaries(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	if got := sc.LookupSummaries("", nil); got != nil {
		t.Errorf("empty claudeDir: %v, want nil", got)
	}
	if got := sc.LookupSummaries("/some/dir", nil); got != nil {
		t.Errorf("empty sessions: %v, want nil", got)
	}

	const cwd = "/tmp/multi"
	sid1, sid2 := "aaaabbbb-0000-0000-0000-000000000005", "aaaabbbb-0000-0000-0000-000000000006"
	unlisted, noIndex := "aaaabbbb-0000-0000-0000-000000000002", "aaaabbbb-0000-0000-0000-000000000004"
	claudeDir, projDir, _ := projectFor(t, cwd, sid1, "")
	writeSessionsIndex(t, projDir, sessionsIndex{OriginalPath: cwd, Entries: []sessionsIndexEntry{
		{SessionID: sid1, Summary: "summary one"},
		{SessionID: sid2, Summary: "summary two"},
	}})
	sessions := map[string]string{sid1: cwd, sid2: cwd, unlisted: cwd, noIndex: "/tmp/no-index"}
	for call := 1; call <= 2; call++ {
		got := sc.LookupSummaries(claudeDir, sessions)
		if len(got) != 2 || got[sid1] != "summary one" || got[sid2] != "summary two" {
			t.Errorf("call %d: %v, want only the two indexed summaries", call, got)
		}
	}
}

// ---------------------------------------------------------------------------
// extractUserText
// ---------------------------------------------------------------------------

func TestExtractUserText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "string content",
			raw:  `{"content":"hello user"}`,
			want: "hello user",
		},
		{
			name: "block content",
			raw:  `{"content":[{"type":"text","text":"block text"}]}`,
			want: "block text",
		},
		{
			name: "whitespace only",
			raw:  `{"content":"   "}`,
			want: "",
		},
		{
			name: "no content field",
			raw:  `{"role":"user"}`,
			want: "",
		},
		{
			name: "tool_result blocks only",
			raw:  `{"content":[{"type":"tool_result","text":"ignored"}]}`,
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractUserText(json.RawMessage(tc.raw))
			if got != tc.want {
				t.Errorf("extractUserText = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// extractLastPrompt via extractLastPromptUncached
// ---------------------------------------------------------------------------

func TestExtractLastPromptUncached_Basic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	prompts := []string{"first prompt", "second prompt", "third prompt"}
	makeJSONLWithUserPrompts(t, path, prompts)

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	got := extractLastPromptUncached(path, fi.Size())
	if got != "third prompt" {
		t.Errorf("extractLastPromptUncached = %q, want %q", got, "third prompt")
	}

	empty := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{empty, "/nonexistent/path.jsonl"} {
		if got := extractLastPromptUncached(p, 0); got != "" {
			t.Errorf("extractLastPromptUncached(%s) = %q, want empty", p, got)
		}
	}
}

func TestExtractLastPromptUncached_LargeTailFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "large.jsonl")

	// Write an initial user prompt, then pad with dummy lines to push it
	// beyond the 512KB tail window so the fallback re-scan is exercised.
	// We use a compact tool_result-like line that has "type":"user" to fool
	// the bytes.Contains check, but whose extractUserText returns empty.
	// The real user text prompt at the head should be found via fallback.
	var sb strings.Builder
	// First line: valid user prompt near the beginning
	sb.WriteString(userJSONLLine("user", "early prompt") + "\n")

	// Fill up more than 512KB with non-text user lines so that the tail scan
	// returns empty and the function falls back to start-of-file.
	// We intentionally construct lines that look like user messages but have
	// no extractable text (tool_result blocks only).
	noTextMsg, _ := json.Marshal(struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}{
		Role:    "user",
		Content: json.RawMessage(`[{"type":"tool_result","content":"ignored"}]`),
	})
	toolResultLine := fmt.Sprintf(`{"type":"user","timestamp":"2026-01-01T01:00:00Z","message":%s}`, string(noTextMsg))

	// Repeat until we exceed 512KB
	for sb.Len() < 600*1024 {
		sb.WriteString(toolResultLine)
		sb.WriteByte('\n')
	}

	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	got := extractLastPromptUncached(path, fi.Size())
	if got != "early prompt" {
		t.Errorf("fallback scan got %q, want early prompt", got)
	}
}

// TestExtractLastPromptUncached_SkipsSystemInjectedXML verifies that
// Claude-Code-injected synthetic user messages (e.g. <task-notification>,
// <system-reminder>) do not leak into the session's last_prompt, which
// would otherwise surface as the session card title. The last real user
// prompt must win even when injected frames appear after it. Regression
// guard for the "card shows <task-notification>" UX bug.
func TestExtractLastPromptUncached_SkipsSystemInjectedXML(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	realPrompt := "比较 NIXL 和 Mooncake 的 EFA 性能"
	injected := []string{
		"<task-notification>\n<task-id>abc</task-id>\n<status>completed</status>\n</task-notification>",
		"<system-reminder>ignore me</system-reminder>",
		"<local-command>/do something</local-command>",
		"<command-name>foo</command-name>",
		"<available-deferred-tools>x,y,z</available-deferred-tools>",
	}

	// The real prompt first, then the system-injected synthetic user messages.
	makeJSONLWithUserPrompts(t, path, append([]string{realPrompt}, injected...))

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	got := extractLastPromptUncached(path, fi.Size())
	if got != realPrompt {
		t.Errorf("extractLastPromptUncached = %q, want %q (injected frames must be skipped)", got, realPrompt)
	}
}

// TestIsClaudeSystemInjectedText exercises the tag-prefix matcher used by
// scanUserPrompt and the history loaders. Keep the whitelist in sync with
// the UI filter in internal/server/static/dashboard.js.
func TestIsClaudeSystemInjectedText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"task-notification close", "<task-notification>x</task-notification>", true},
		{"task-notification with space", "<task-notification attr=\"v\">x</task-notification>", true},
		{"task-notification with newline", "<task-notification\nx></task-notification>", true},
		{"system-reminder", "<system-reminder>foo</system-reminder>", true},
		{"local-command", "<local-command>/do</local-command>", true},
		{"command-name", "<command-name>foo</command-name>", true},
		{"available-deferred-tools", "<available-deferred-tools>x</available-deferred-tools>", true},
		{"interrupt marker", "[Request interrupted by user]", true},
		{"interrupt marker tool use", "[Request interrupted by user for tool use]", true},
		{"interrupt marker with trailing text", "[Request interrupted by user] plus user reply", false},
		{"real user text starting with <", "<think> this is a thought", false},
		{"real user text starting with angle", "<html> tag in a question", false},
		{"empty", "", false},
		{"unrelated tag", "<foo>bar</foo>", false},
		{"similar prefix but not exact", "<task-notifications>x</task-notifications>", false},
		{"plain text", "hello world", false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsClaudeSystemInjectedText(tc.in); got != tc.want {
				t.Errorf("IsClaudeSystemInjectedText(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// extractLastPrompt cache behaviour
// ---------------------------------------------------------------------------

// TestExtractLastPrompt_Cache: a read fills the cache under the file's mtime,
// and a rewrite (a new mtime) is read afresh.
func TestExtractLastPrompt_Cache(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	const cwd, sessionID = "/tmp/cache-hit", "ccccdddd-0000-0000-0000-000000000001"
	claudeDir, _, jsonlPath := projectFor(t, cwd, sessionID, "old prompt")

	if got := sc.extractLastPrompt(claudeDir, cwd, sessionID); got != "old prompt" {
		t.Errorf("first call = %q, want old prompt", got)
	}
	fi, _ := os.Stat(jsonlPath)
	if cached, ok := sc.getCachedPrompt(jsonlPath, fi.ModTime().UnixNano()); !ok || cached != "old prompt" {
		t.Errorf("cache after the first call = (%q, %v), want (old prompt, true)", cached, ok)
	}

	time.Sleep(10 * time.Millisecond) // a different mtime on most filesystems
	makeJSONLWithUserPrompts(t, jsonlPath, []string{"new prompt"})
	if got := sc.extractLastPrompt(claudeDir, cwd, sessionID); got != "new prompt" {
		t.Errorf("after file update = %q, want new prompt", got)
	}
}

// ---------------------------------------------------------------------------
// evictPromptCache and evictSummaryCache
// ---------------------------------------------------------------------------

// Eviction tests now operate on an isolated *Scanner so they can run in
// parallel — previously they contended on the package globals.
func TestEvictPromptCache_UnderThreshold(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	sc.promptCache.Lock()
	// Fill with 10 entries — well under the 500 threshold
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("path%d", i)
		sc.promptCache.entries[key] = newPromptEntry(1, "x", 0)
	}
	sc.promptCache.generation.Store(5)
	beforeLen := len(sc.promptCache.entries)
	sc.evictPromptCache() // should be a no-op
	afterLen := len(sc.promptCache.entries)
	sc.promptCache.Unlock()

	if beforeLen != afterLen {
		t.Errorf("evictPromptCache should not evict under threshold: before=%d after=%d", beforeLen, afterLen)
	}
}

func TestEvictPromptCache_OverThreshold(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	sc.promptCache.Lock()
	// Fill with 501 entries with old generation
	for i := 0; i < 501; i++ {
		key := fmt.Sprintf("path%d", i)
		sc.promptCache.entries[key] = newPromptEntry(1, "x", 0)
	}
	// Add one entry with current generation
	sc.promptCache.entries["current"] = newPromptEntry(1, "y", 3)
	sc.promptCache.generation.Store(3)
	sc.evictPromptCache()
	afterLen := len(sc.promptCache.entries)
	_, hasCurrent := sc.promptCache.entries["current"]
	sc.promptCache.Unlock()

	if afterLen >= 502 {
		t.Errorf("expected eviction to reduce entries, still have %d", afterLen)
	}
	if !hasCurrent {
		t.Error("current-generation entry should not be evicted")
	}
}

func TestEvictSummaryCache_OverThreshold(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	sc.summaryCache.Lock()
	for i := 0; i < 501; i++ {
		key := fmt.Sprintf("idx%d", i)
		sc.summaryCache.entries[key] = newSummaryEntry(1, sessionsIndex{}, 0)
	}
	sc.summaryCache.entries["current"] = newSummaryEntry(1, sessionsIndex{}, 3)
	sc.summaryCache.generation.Store(3)
	sc.evictSummaryCache()
	afterLen := len(sc.summaryCache.entries)
	_, hasCurrent := sc.summaryCache.entries["current"]
	sc.summaryCache.Unlock()

	if afterLen >= 502 {
		t.Errorf("expected eviction to reduce entries, still have %d", afterLen)
	}
	if !hasCurrent {
		t.Error("current-generation entry should not be evicted")
	}
}

// ---------------------------------------------------------------------------
// RefreshDynamic
// ---------------------------------------------------------------------------

func TestRefreshDynamic_EmptyInputs(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	changed := sc.RefreshDynamic("", nil)
	if changed {
		t.Error("expected false for empty claudeDir")
	}
	changed = sc.RefreshDynamic("/some/dir", []DiscoveredSession{})
	if changed {
		t.Error("expected false for empty sessions slice")
	}
}

// TestRefreshDynamic_LastActivePromptAndSummary: a refresh takes LastActive
// from the JSONL's mtime, the prompt from its last user line and the summary
// from the index, and reports the change; a second refresh with nothing new
// reports none.
func TestRefreshDynamic_LastActivePromptAndSummary(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	const cwd, sessionID = "/tmp/refresh-dynamic", "eeeeffff-0000-0000-0000-000000000001"
	claudeDir, projDir, jsonlPath := projectFor(t, cwd, sessionID, "refresh me")
	writeSessionsIndex(t, projDir, sessionsIndex{OriginalPath: cwd, Entries: []sessionsIndexEntry{
		{SessionID: sessionID, Summary: "freshly computed summary"},
	}})
	fi, err := os.Stat(jsonlPath)
	if err != nil {
		t.Fatal(err)
	}

	sessions := []DiscoveredSession{{SessionID: sessionID, CWD: cwd, State: "ready"}}
	if !sc.RefreshDynamic(claudeDir, sessions) {
		t.Error("expected changed=true after refresh")
	}
	s := sessions[0]
	if s.LastActive != fi.ModTime().UnixMilli() || s.LastPrompt != "refresh me" || s.Summary != "freshly computed summary" {
		t.Errorf("after refresh: LastActive %d (want %d), prompt %q, summary %q",
			s.LastActive, fi.ModTime().UnixMilli(), s.LastPrompt, s.Summary)
	}
	if sc.RefreshDynamic(claudeDir, sessions) {
		t.Error("expected changed=false when nothing changed")
	}
}

// R20260603-PERF-2: extractLastPromptWithMtime returns the JSONL mtime so the
// caller can avoid a second os.Stat. Verify the mtime matches a direct Stat
// and that missing files report ok=false.
func TestExtractLastPromptWithMtime(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	const cwd, sessionID = "/tmp/extract-mtime", "eeeeffff-0000-0000-0000-0000000000cd"
	claudeDir, _, jsonlPath := projectFor(t, cwd, sessionID, "hello mtime")
	fi, err := os.Stat(jsonlPath)
	if err != nil {
		t.Fatal(err)
	}

	prompt, mtime, ok := sc.extractLastPromptWithMtime(claudeDir, cwd, sessionID)
	if !ok || prompt != "hello mtime" || mtime != fi.ModTime().UnixMilli() {
		t.Errorf("= (%q, %d, %v), want (hello mtime, %d, true)", prompt, mtime, ok, fi.ModTime().UnixMilli())
	}
	if _, _, ok := sc.extractLastPromptWithMtime(claudeDir, "/tmp/nope", "00000000-0000-0000-0000-000000000000"); ok {
		t.Error("expected ok=false for missing JSONL")
	}
}

// R20260603-PERF-2: RefreshDynamic reuses the mtime captured during prompt
// extraction. When the JSONL is missing, LastActive must fall back to
// StartedAt.
func TestRefreshDynamic_MissingJSONLFallsBackToStartedAt(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	claudeDir := makeClaudeDir(t)
	sessions := []DiscoveredSession{
		{
			SessionID:  "eeeeffff-0000-0000-0000-0000000000ab",
			CWD:        "/tmp/refresh-dynamic-missing",
			StartedAt:  424242,
			LastActive: 0,
			State:      "ready",
		},
	}
	changed := sc.RefreshDynamic(claudeDir, sessions)
	if !changed {
		t.Error("expected changed=true (LastActive moved from 0 to StartedAt)")
	}
	if sessions[0].LastActive != 424242 {
		t.Errorf("LastActive = %d, want StartedAt 424242", sessions[0].LastActive)
	}
	if sessions[0].LastPrompt != "" {
		t.Errorf("LastPrompt = %q, want empty for missing JSONL", sessions[0].LastPrompt)
	}
}
