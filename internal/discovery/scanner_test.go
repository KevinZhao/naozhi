package discovery

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
	sc.promptCache.entries = make(map[string]promptCacheEntry)
	sc.promptCache.generation = 0
	sc.promptCache.Unlock()

	sc.summaryCache.Lock()
	sc.summaryCache.entries = make(map[string]summaryCacheEntry)
	sc.summaryCache.generation = 0
	sc.summaryCache.Unlock()

	t.Cleanup(func() {
		sc := DefaultScanner()
		sc.promptCache.Lock()
		sc.promptCache.entries = make(map[string]promptCacheEntry)
		sc.promptCache.generation = 0
		sc.promptCache.Unlock()

		sc.summaryCache.Lock()
		sc.summaryCache.entries = make(map[string]summaryCacheEntry)
		sc.summaryCache.generation = 0
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

// ---------------------------------------------------------------------------
// IsValidSessionID
// ---------------------------------------------------------------------------

func TestIsValidSessionID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"valid lowercase", "550e8400-e29b-41d4-a716-446655440000", true},
		{"valid v4 style", "00000000-0000-4000-8000-000000000000", true},
		{"empty", "", false},
		{"no hyphens", "550e8400e29b41d4a716446655440000", false},
		{"too short", "550e8400-e29b-41d4-a716-44665544000", false},
		{"uppercase", "550E8400-E29B-41D4-A716-446655440000", false},
		{"extra char", "550e8400-e29b-41d4-a716-4466554400001", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := IsValidSessionID(tc.input)
			if got != tc.want {
				t.Errorf("IsValidSessionID(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// projDirName
// ---------------------------------------------------------------------------

func TestProjDirName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		cwd  string
		want string
	}{
		{"/home/user/workspace/foo", "-home-user-workspace-foo"},
		{"/tmp", "-tmp"},
		{"", ""},
	}
	for _, tc := range tests {
		t.Run(tc.cwd, func(t *testing.T) {
			t.Parallel()
			got := projDirName(tc.cwd)
			if got != tc.want {
				t.Errorf("projDirName(%q) = %q, want %q", tc.cwd, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// jsonlMtime
// ---------------------------------------------------------------------------

func TestJsonlMtime_Fallback(t *testing.T) {
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	// JSONL does not exist: should return startedAt fallback
	got := jsonlMtime(claudeDir, "/nonexistent/path", "00000000-0000-0000-0000-000000000001", 12345)
	if got != 12345 {
		t.Errorf("jsonlMtime fallback = %d, want 12345", got)
	}
}

func TestJsonlMtime_ReadsFile(t *testing.T) {
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/test-mtime"
	sessionID := "00000000-0000-0000-0000-000000000002"
	dirName := projDirName(cwd)
	projDir := filepath.Join(claudeDir, "projects", dirName)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonlPath := filepath.Join(projDir, sessionID+".jsonl")
	if err := os.WriteFile(jsonlPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	beforeMs := time.Now().UnixMilli()
	got := jsonlMtime(claudeDir, cwd, sessionID, 0)
	afterMs := time.Now().UnixMilli()

	if got < beforeMs-1000 || got > afterMs+1000 {
		t.Errorf("jsonlMtime = %d, expected value around %d", got, beforeMs)
	}
}

// ---------------------------------------------------------------------------
// LookupSummaries
// ---------------------------------------------------------------------------

func TestLookupSummaries_Empty(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	got := sc.LookupSummaries("", nil)
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
	got = sc.LookupSummaries("/some/dir", nil)
	if got != nil {
		t.Errorf("expected nil for empty sessions map, got %v", got)
	}
}

func TestLookupSummaries_BasicLookup(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/lookup-project"
	sid := "aaaabbbb-0000-0000-0000-000000000001"
	dirName := projDirName(cwd)
	projDir := filepath.Join(claudeDir, "projects", dirName)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSessionsIndex(t, projDir, sessionsIndex{
		OriginalPath: cwd,
		Entries: []sessionsIndexEntry{
			{SessionID: sid, Summary: "My session summary"},
		},
	})

	got := sc.LookupSummaries(claudeDir, map[string]string{sid: cwd})
	if got[sid] != "My session summary" {
		t.Errorf("LookupSummaries[%q] = %q, want %q", sid, got[sid], "My session summary")
	}
}

func TestLookupSummaries_NotInIndex(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/lookup-missing"
	sid := "aaaabbbb-0000-0000-0000-000000000002"
	dirName := projDirName(cwd)
	projDir := filepath.Join(claudeDir, "projects", dirName)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSessionsIndex(t, projDir, sessionsIndex{
		OriginalPath: cwd,
		Entries:      []sessionsIndexEntry{},
	})

	got := sc.LookupSummaries(claudeDir, map[string]string{sid: cwd})
	if val, ok := got[sid]; ok {
		t.Errorf("expected no entry for missing session, got %q", val)
	}
}

func TestLookupSummaries_CacheHit(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/cache-test"
	sid := "aaaabbbb-0000-0000-0000-000000000003"
	dirName := projDirName(cwd)
	projDir := filepath.Join(claudeDir, "projects", dirName)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSessionsIndex(t, projDir, sessionsIndex{
		Entries: []sessionsIndexEntry{
			{SessionID: sid, Summary: "cached summary"},
		},
	})

	// First call: cache miss
	got1 := sc.LookupSummaries(claudeDir, map[string]string{sid: cwd})
	if got1[sid] != "cached summary" {
		t.Errorf("first call: got %q, want cached summary", got1[sid])
	}

	// Second call on same mtime: cache hit
	got2 := sc.LookupSummaries(claudeDir, map[string]string{sid: cwd})
	if got2[sid] != "cached summary" {
		t.Errorf("second call (cache hit): got %q, want cached summary", got2[sid])
	}
}

func TestLookupSummaries_MissingIndexFile(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	claudeDir := makeClaudeDir(t)
	sid := "aaaabbbb-0000-0000-0000-000000000004"
	cwd := "/tmp/no-index"
	// Don't create any directory or index file
	got := sc.LookupSummaries(claudeDir, map[string]string{sid: cwd})
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

func TestLookupSummaries_MultipleSessionsSameProject(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/multi"
	sid1 := "aaaabbbb-0000-0000-0000-000000000005"
	sid2 := "aaaabbbb-0000-0000-0000-000000000006"
	dirName := projDirName(cwd)
	projDir := filepath.Join(claudeDir, "projects", dirName)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSessionsIndex(t, projDir, sessionsIndex{
		OriginalPath: cwd,
		Entries: []sessionsIndexEntry{
			{SessionID: sid1, Summary: "summary one"},
			{SessionID: sid2, Summary: "summary two"},
		},
	})

	sessions := map[string]string{sid1: cwd, sid2: cwd}
	got := sc.LookupSummaries(claudeDir, sessions)
	if got[sid1] != "summary one" {
		t.Errorf("sid1 summary = %q, want summary one", got[sid1])
	}
	if got[sid2] != "summary two" {
		t.Errorf("sid2 summary = %q, want summary two", got[sid2])
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
}

func TestExtractLastPromptUncached_EmptyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	got := extractLastPromptUncached(path, 0)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestExtractLastPromptUncached_NonexistentFile(t *testing.T) {
	t.Parallel()
	got := extractLastPromptUncached("/nonexistent/path.jsonl", 0)
	if got != "" {
		t.Errorf("expected empty for nonexistent file, got %q", got)
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
	msg, _ := json.Marshal(struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{Role: "user", Content: "early prompt"})
	sb.WriteString(fmt.Sprintf(`{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":%s}`+"\n", string(msg)))

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

	var lines []string
	// Real prompt first.
	msg, _ := json.Marshal(struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{Role: "user", Content: realPrompt})
	lines = append(lines, fmt.Sprintf(`{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":%s}`, string(msg)))
	// Then a series of system-injected synthetic user messages.
	for i, inj := range injected {
		m, _ := json.Marshal(struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{Role: "user", Content: inj})
		lines = append(lines, fmt.Sprintf(`{"type":"user","timestamp":"2026-01-01T%02d:00:00Z","message":%s}`, i+1, string(m)))
	}
	writeJSONLFile(t, path, lines)

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

func TestExtractLastPrompt_CacheHit(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/cache-hit"
	sessionID := "ccccdddd-0000-0000-0000-000000000001"
	dirName := projDirName(cwd)
	projDir := filepath.Join(claudeDir, "projects", dirName)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonlPath := filepath.Join(projDir, sessionID+".jsonl")
	makeJSONLWithUserPrompts(t, jsonlPath, []string{"cached prompt"})

	// First call: cache miss → reads file
	got1 := sc.extractLastPrompt(claudeDir, cwd, sessionID)
	if got1 != "cached prompt" {
		t.Errorf("first call = %q, want cached prompt", got1)
	}
	// Verify cache was populated
	fi, _ := os.Stat(jsonlPath)
	mtime := fi.ModTime().UnixNano()
	cached, ok := sc.getCachedPrompt(jsonlPath, mtime)
	if !ok {
		t.Error("expected cache hit after first extractLastPrompt call")
	}
	if cached != "cached prompt" {
		t.Errorf("cached value = %q, want cached prompt", cached)
	}

	// Second call: cache hit (no file read needed)
	got2 := sc.extractLastPrompt(claudeDir, cwd, sessionID)
	if got2 != "cached prompt" {
		t.Errorf("second call = %q, want cached prompt", got2)
	}
}

func TestExtractLastPrompt_CacheInvalidatedOnMtimeChange(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/cache-invalidate"
	sessionID := "ccccdddd-0000-0000-0000-000000000002"
	dirName := projDirName(cwd)
	projDir := filepath.Join(claudeDir, "projects", dirName)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonlPath := filepath.Join(projDir, sessionID+".jsonl")
	makeJSONLWithUserPrompts(t, jsonlPath, []string{"old prompt"})

	got1 := sc.extractLastPrompt(claudeDir, cwd, sessionID)
	if got1 != "old prompt" {
		t.Errorf("first call = %q, want old prompt", got1)
	}

	// Overwrite file (different mtime guaranteed on most filesystems)
	time.Sleep(10 * time.Millisecond)
	makeJSONLWithUserPrompts(t, jsonlPath, []string{"new prompt"})

	got2 := sc.extractLastPrompt(claudeDir, cwd, sessionID)
	if got2 != "new prompt" {
		t.Errorf("after file update = %q, want new prompt", got2)
	}
}

// ---------------------------------------------------------------------------
// listJSONLsByMtime
// ---------------------------------------------------------------------------

func TestListJSONLsByMtime_Ordering(t *testing.T) {
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/order-test"
	dirName := projDirName(cwd)
	projDir := filepath.Join(claudeDir, "projects", dirName)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write files with explicit sleep to ensure distinct mtimes
	sid1 := "aaaaaaaa-0000-0000-0000-000000000001"
	sid2 := "aaaaaaaa-0000-0000-0000-000000000002"

	if err := os.WriteFile(filepath.Join(projDir, sid1+".jsonl"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(projDir, sid2+".jsonl"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := listJSONLsByMtime(claudeDir, cwd)
	if len(result) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(result))
	}
	// Newest first
	if result[0].id != sid2 {
		t.Errorf("first entry = %q, want %q (newest first)", result[0].id, sid2)
	}
	if result[1].id != sid1 {
		t.Errorf("second entry = %q, want %q", result[1].id, sid1)
	}
}

func TestListJSONLsByMtime_SkipNonJSONL(t *testing.T) {
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/skip-test"
	dirName := projDirName(cwd)
	projDir := filepath.Join(claudeDir, "projects", dirName)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write a valid JSONL and an unrelated file
	sid := "bbbbbbbb-0000-0000-0000-000000000001"
	if err := os.WriteFile(filepath.Join(projDir, sid+".jsonl"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "sessions-index.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := listJSONLsByMtime(claudeDir, cwd)
	if len(result) != 1 {
		t.Fatalf("expected 1 entry (only .jsonl files), got %d", len(result))
	}
	if result[0].id != sid {
		t.Errorf("entry id = %q, want %q", result[0].id, sid)
	}
}

// ---------------------------------------------------------------------------
// findJSONLPath
// ---------------------------------------------------------------------------

func TestFindJSONLPath(t *testing.T) {
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/find-jsonl"
	sessionID := "ddddeeee-0000-0000-0000-000000000001"
	dirName := projDirName(cwd)
	projDir := filepath.Join(claudeDir, "projects", dirName)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(projDir, sessionID+".jsonl")
	if err := os.WriteFile(expected, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := findJSONLPath(claudeDir, cwd, sessionID)
	if got != expected {
		t.Errorf("findJSONLPath = %q, want %q", got, expected)
	}

	// Missing file
	got2 := findJSONLPath(claudeDir, "/nonexistent/cwd", "missing-session")
	if got2 != "" {
		t.Errorf("expected empty for missing file, got %q", got2)
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
		sc.promptCache.entries[key] = promptCacheEntry{mtime: 1, prompt: "x", gen: 0}
	}
	sc.promptCache.generation = 5
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
		sc.promptCache.entries[key] = promptCacheEntry{mtime: 1, prompt: "x", gen: 0}
	}
	// Add one entry with current generation
	sc.promptCache.entries["current"] = promptCacheEntry{mtime: 1, prompt: "y", gen: 3}
	sc.promptCache.generation = 3
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
		sc.summaryCache.entries[key] = summaryCacheEntry{mtime: 1, index: sessionsIndex{}, gen: 0}
	}
	sc.summaryCache.entries["current"] = summaryCacheEntry{mtime: 1, index: sessionsIndex{}, gen: 3}
	sc.summaryCache.generation = 3
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

func TestRefreshDynamic_LastActiveAndState(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/refresh-dynamic"
	sessionID := "eeeeffff-0000-0000-0000-000000000001"
	dirName := projDirName(cwd)
	projDir := filepath.Join(claudeDir, "projects", dirName)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonlPath := filepath.Join(projDir, sessionID+".jsonl")
	makeJSONLWithUserPrompts(t, jsonlPath, []string{"refresh me"})

	fi, err := os.Stat(jsonlPath)
	if err != nil {
		t.Fatal(err)
	}
	expectedMtime := fi.ModTime().UnixMilli()

	sessions := []DiscoveredSession{
		{
			SessionID:  sessionID,
			CWD:        cwd,
			LastActive: 0, // stale
			State:      "ready",
			LastPrompt: "",
		},
	}

	changed := sc.RefreshDynamic(claudeDir, sessions)
	if !changed {
		t.Error("expected changed=true after refresh")
	}
	if sessions[0].LastActive != expectedMtime {
		t.Errorf("LastActive = %d, want %d", sessions[0].LastActive, expectedMtime)
	}
	if sessions[0].LastPrompt != "refresh me" {
		t.Errorf("LastPrompt = %q, want refresh me", sessions[0].LastPrompt)
	}
}

func TestRefreshDynamic_SummaryUpdated(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/refresh-summary"
	sessionID := "eeeeffff-0000-0000-0000-000000000002"
	dirName := projDirName(cwd)
	projDir := filepath.Join(claudeDir, "projects", dirName)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// JSONL needed so jsonlMtime works
	jsonlPath := filepath.Join(projDir, sessionID+".jsonl")
	makeJSONLWithUserPrompts(t, jsonlPath, []string{"hello"})

	writeSessionsIndex(t, projDir, sessionsIndex{
		OriginalPath: cwd,
		Entries: []sessionsIndexEntry{
			{SessionID: sessionID, Summary: "freshly computed summary"},
		},
	})

	sessions := []DiscoveredSession{
		{
			SessionID: sessionID,
			CWD:       cwd,
			Summary:   "", // no summary yet
		},
	}

	changed := sc.RefreshDynamic(claudeDir, sessions)
	if !changed {
		t.Error("expected changed=true when summary is added")
	}
	if sessions[0].Summary != "freshly computed summary" {
		t.Errorf("Summary = %q, want freshly computed summary", sessions[0].Summary)
	}
}

func TestRefreshDynamic_NoChanges(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/no-change"
	sessionID := "eeeeffff-0000-0000-0000-000000000003"
	dirName := projDirName(cwd)
	projDir := filepath.Join(claudeDir, "projects", dirName)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonlPath := filepath.Join(projDir, sessionID+".jsonl")
	makeJSONLWithUserPrompts(t, jsonlPath, []string{"stable prompt"})
	fi, _ := os.Stat(jsonlPath)
	mtime := fi.ModTime().UnixMilli()

	// Since the JSONL was just written, its mtime is "now" and state should
	// be "running" (within the 30s runningThreshold window).
	sessions := []DiscoveredSession{
		{
			SessionID:  sessionID,
			CWD:        cwd,
			LastActive: mtime,
			State:      "running",
			LastPrompt: "stable prompt",
			Summary:    "",
		},
	}

	changed := sc.RefreshDynamic(claudeDir, sessions)
	if changed {
		t.Error("expected changed=false when nothing changed")
	}
}

// ---------------------------------------------------------------------------
// setCachedPrompt / getCachedPrompt round-trip
// ---------------------------------------------------------------------------

func TestPromptCacheRoundTrip(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	const path = "/test/path.jsonl"
	const mtime = int64(99999)
	const prompt = "my cached prompt"

	sc.setCachedPrompt(path, mtime, prompt)

	got, ok := sc.getCachedPrompt(path, mtime)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got != prompt {
		t.Errorf("got %q, want %q", got, prompt)
	}

	// Wrong mtime → miss
	_, ok2 := sc.getCachedPrompt(path, mtime+1)
	if ok2 {
		t.Error("expected cache miss for different mtime")
	}
}

// ---------------------------------------------------------------------------
// setCachedSummary / getCachedSummary round-trip
// ---------------------------------------------------------------------------

func TestSummaryCacheRoundTrip(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	const path = "/test/sessions-index.json"
	const mtime = int64(12345)
	idx := sessionsIndex{
		OriginalPath: "/some/path",
		Entries:      []sessionsIndexEntry{{SessionID: "abc", Summary: "sum"}},
	}

	sc.setCachedSummary(path, mtime, idx)

	got, ok := sc.getCachedSummary(path, mtime)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if len(got.Entries) != 1 || got.Entries[0].Summary != "sum" {
		t.Errorf("unexpected cached index: %+v", got)
	}

	// Wrong mtime → miss
	_, ok2 := sc.getCachedSummary(path, mtime+1)
	if ok2 {
		t.Error("expected cache miss for different mtime")
	}
}
