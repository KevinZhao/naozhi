package discovery

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// resolveWorkspaceByParts
// ---------------------------------------------------------------------------

func TestResolveWorkspaceByParts_RealDir(t *testing.T) {
	t.Parallel()
	// Use t.TempDir() so the path definitely exists.
	base := t.TempDir()
	// encoded: replace "/" with "-"
	encoded := "-" + base[1:] // strip leading "/" and prepend "-"
	// Replace all "/" in the remainder with "-"
	for i := 0; i < len(encoded); i++ {
		if encoded[i] == '/' {
			encoded = encoded[:i] + "-" + encoded[i+1:]
		}
	}

	// Clear dfsPathCache for this key so we always do a fresh resolution.
	dfsPathCache.Delete(encoded)
	t.Cleanup(func() { dfsPathCache.Delete(encoded) })

	got := resolveWorkspaceByParts(encoded)
	if got != base {
		t.Errorf("resolveWorkspaceByParts(%q) = %q, want %q", encoded, got, base)
	}
}

func TestResolveWorkspaceByParts_Cache(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	encoded := "-" + base[1:]
	for i := 0; i < len(encoded); i++ {
		if encoded[i] == '/' {
			encoded = encoded[:i] + "-" + encoded[i+1:]
		}
	}
	dfsPathCache.Delete(encoded)
	t.Cleanup(func() { dfsPathCache.Delete(encoded) })

	// First call populates cache.
	got1 := resolveWorkspaceByParts(encoded)
	// Second call returns from cache.
	got2 := resolveWorkspaceByParts(encoded)
	if got1 != got2 {
		t.Errorf("cache inconsistency: %q vs %q", got1, got2)
	}
}

func TestResolveWorkspaceByParts_NonexistentPath(t *testing.T) {
	t.Parallel()
	// Encode a path that doesn't exist on disk.
	encoded := "-nonexistent-path-that-cannot-exist-xyz987"
	dfsPathCache.Delete(encoded)
	t.Cleanup(func() { dfsPathCache.Delete(encoded) })

	got := resolveWorkspaceByParts(encoded)
	if got != "" {
		t.Errorf("expected empty for nonexistent path, got %q", got)
	}
}

func TestResolveWorkspaceByParts_EmptyAndNoLeadingDash(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"no leading dash", "home-user-project"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dfsPathCache.Delete(tc.input)
			t.Cleanup(func() { dfsPathCache.Delete(tc.input) })
			got := resolveWorkspaceByParts(tc.input)
			if got != "" {
				t.Errorf("resolveWorkspaceByParts(%q) = %q, want empty", tc.input, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// cachedJSONLFileInfo
// ---------------------------------------------------------------------------

func TestCachedJSONLFileInfo_Basic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid1 := "aaaaaaaa-0001-0001-0001-000000000001"
	sid2 := "aaaaaaaa-0001-0001-0001-000000000002"

	if err := os.WriteFile(filepath.Join(dir, sid1+".jsonl"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sid2+".jsonl"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A zero-size file should be excluded.
	if err := os.WriteFile(filepath.Join(dir, "empty.jsonl"), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	// A non-.jsonl file should be excluded.
	if err := os.WriteFile(filepath.Join(dir, "sessions-index.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	dirFilesCache.Delete(dir)
	t.Cleanup(func() { dirFilesCache.Delete(dir) })

	files := cachedJSONLFileInfo(dir)
	if len(files) != 2 {
		t.Fatalf("expected 2 files (non-empty .jsonl only), got %d", len(files))
	}

	ids := map[string]bool{}
	for _, f := range files {
		ids[f.sessionID] = true
	}
	if !ids[sid1] || !ids[sid2] {
		t.Errorf("unexpected session IDs: %v", ids)
	}
}

func TestCachedJSONLFileInfo_CacheHitAfterRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "aaaaaaaa-0001-0001-0001-000000000003"
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	dirFilesCache.Delete(dir)
	t.Cleanup(func() { dirFilesCache.Delete(dir) })

	// First call — cache miss
	files1 := cachedJSONLFileInfo(dir)
	// Second call — cache hit (directory hasn't changed)
	files2 := cachedJSONLFileInfo(dir)
	if len(files1) != len(files2) {
		t.Errorf("cache inconsistency: %d vs %d", len(files1), len(files2))
	}
}

func TestCachedJSONLFileInfo_NonexistentDir(t *testing.T) {
	t.Parallel()
	dirFilesCache.Delete("/nonexistent/dir")
	t.Cleanup(func() { dirFilesCache.Delete("/nonexistent/dir") })

	files := cachedJSONLFileInfo("/nonexistent/dir")
	if len(files) != 0 {
		t.Errorf("expected empty for nonexistent dir, got %d files", len(files))
	}
}

// ---------------------------------------------------------------------------
// recentFromJSONLFiles
// ---------------------------------------------------------------------------

func TestRecentFromJSONLFiles_Basic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	workspace := "/tmp/test-workspace"
	sid := "bbbbbbbb-0001-0001-0001-000000000001"

	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	dirFilesCache.Delete(dir)
	t.Cleanup(func() { dirFilesCache.Delete(dir) })

	results := recentFromJSONLFiles(dir, workspace, nil)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].SessionID != sid {
		t.Errorf("session ID = %q, want %q", results[0].SessionID, sid)
	}
	if results[0].Workspace != workspace {
		t.Errorf("workspace = %q, want %q", results[0].Workspace, workspace)
	}
}

func TestRecentFromJSONLFiles_ExcludeBySessionID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	workspace := "/tmp/exclude-test"
	sid := "bbbbbbbb-0001-0001-0001-000000000002"

	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	dirFilesCache.Delete(dir)
	t.Cleanup(func() { dirFilesCache.Delete(dir) })

	exclude := map[string]bool{sid: true}
	results := recentFromJSONLFiles(dir, workspace, exclude)
	if len(results) != 0 {
		t.Errorf("expected 0 results when session is excluded, got %d", len(results))
	}
}

func TestRecentFromJSONLFiles_InvalidSessionIDSkipped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	workspace := "/tmp/invalid-sid"

	// Write a file with an invalid session ID (not UUID format)
	if err := os.WriteFile(filepath.Join(dir, "not-a-uuid.jsonl"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Write a valid session
	sid := "cccccccc-0001-0001-0001-000000000001"
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	dirFilesCache.Delete(dir)
	t.Cleanup(func() { dirFilesCache.Delete(dir) })

	results := recentFromJSONLFiles(dir, workspace, nil)
	if len(results) != 1 {
		t.Fatalf("expected 1 result (invalid UUID skipped), got %d", len(results))
	}
	if results[0].SessionID != sid {
		t.Errorf("session ID = %q, want %q", results[0].SessionID, sid)
	}
}

// ---------------------------------------------------------------------------
// recentFromParsedIndex
// ---------------------------------------------------------------------------

func TestRecentFromParsedIndex_Basic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	workspace := "/tmp/index-project"
	sid := "dddddddd-0001-0001-0001-000000000001"

	// Need a real JSONL file so cachedJSONLFileInfo finds it.
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	dirFilesCache.Delete(dir)
	t.Cleanup(func() { dirFilesCache.Delete(dir) })

	idx := &sessionsIndex{
		OriginalPath: workspace,
		Entries: []sessionsIndexEntry{
			{SessionID: sid, Summary: "index summary", FirstPrompt: "first prompt from index"},
		},
	}

	results := recentFromParsedIndex(idx, dir, workspace, nil)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].SessionID != sid {
		t.Errorf("session ID = %q, want %q", results[0].SessionID, sid)
	}
	if results[0].Summary != "index summary" {
		t.Errorf("summary = %q, want index summary", results[0].Summary)
	}
	// LastPrompt should come from FirstPrompt when set
	if results[0].LastPrompt != "first prompt from index" {
		t.Errorf("last prompt = %q, want first prompt from index", results[0].LastPrompt)
	}
}

func TestRecentFromParsedIndex_FallsBackToSummaryForPrompt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	workspace := "/tmp/fallback-prompt"
	sid := "dddddddd-0001-0001-0001-000000000002"

	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	dirFilesCache.Delete(dir)
	t.Cleanup(func() { dirFilesCache.Delete(dir) })

	idx := &sessionsIndex{
		OriginalPath: workspace,
		Entries: []sessionsIndexEntry{
			// FirstPrompt is empty → should use Summary as prompt
			{SessionID: sid, Summary: "fallback to summary", FirstPrompt: ""},
		},
	}

	results := recentFromParsedIndex(idx, dir, workspace, nil)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].LastPrompt != "fallback to summary" {
		t.Errorf("last prompt = %q, want fallback to summary", results[0].LastPrompt)
	}
}

func TestRecentFromParsedIndex_SkipMissingJSONL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	workspace := "/tmp/missing-jsonl"
	sid := "dddddddd-0001-0001-0001-000000000003"
	// No JSONL file written — index entry should be skipped.

	dirFilesCache.Delete(dir)
	t.Cleanup(func() { dirFilesCache.Delete(dir) })

	idx := &sessionsIndex{
		Entries: []sessionsIndexEntry{{SessionID: sid, Summary: "ghost session"}},
	}
	results := recentFromParsedIndex(idx, dir, workspace, nil)
	if len(results) != 0 {
		t.Errorf("expected 0 results for missing JSONL, got %d", len(results))
	}
}

func TestRecentFromParsedIndex_ExcludeBySessionID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	workspace := "/tmp/exclude-idx"
	sid := "dddddddd-0001-0001-0001-000000000004"

	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	dirFilesCache.Delete(dir)
	t.Cleanup(func() { dirFilesCache.Delete(dir) })

	idx := &sessionsIndex{
		Entries: []sessionsIndexEntry{{SessionID: sid, Summary: "excluded"}},
	}
	results := recentFromParsedIndex(idx, dir, workspace, map[string]bool{sid: true})
	if len(results) != 0 {
		t.Errorf("expected 0 results for excluded session, got %d", len(results))
	}
}

// ---------------------------------------------------------------------------
// extractFirstPrompt
// ---------------------------------------------------------------------------

func TestExtractFirstPrompt_Basic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	msg, _ := json.Marshal(struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{Role: "user", Content: "first user prompt"})
	line := fmt.Sprintf(`{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":%s}`, string(msg))
	writeJSONLFile(t, path, []string{line})

	got := extractFirstPrompt(path)
	if got != "first user prompt" {
		t.Errorf("extractFirstPrompt = %q, want first user prompt", got)
	}
}

func TestExtractFirstPrompt_ReturnsFirstNotLast(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "multi.jsonl")

	prompts := []string{"first", "second", "third"}
	var lines []string
	for i, p := range prompts {
		ts := fmt.Sprintf("2026-01-01T%02d:00:00Z", i)
		msg, _ := json.Marshal(struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{Role: "user", Content: p})
		lines = append(lines, fmt.Sprintf(`{"type":"user","timestamp":%q,"message":%s}`, ts, string(msg)))
	}
	writeJSONLFile(t, path, lines)

	got := extractFirstPrompt(path)
	if got != "first" {
		t.Errorf("extractFirstPrompt = %q, want first", got)
	}
}

func TestExtractFirstPrompt_NonexistentFile(t *testing.T) {
	t.Parallel()
	got := extractFirstPrompt("/nonexistent/session.jsonl")
	if got != "" {
		t.Errorf("expected empty for nonexistent file, got %q", got)
	}
}

func TestExtractFirstPrompt_SkipsNonUserLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "mixed.jsonl")

	// Only an assistant line and then a user line
	assistantLine := assistantJSONLLine("assistant text")
	msg, _ := json.Marshal(struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{Role: "user", Content: "actual user"})
	userLine := fmt.Sprintf(`{"type":"user","timestamp":"2026-01-01T01:00:00Z","message":%s}`, string(msg))

	writeJSONLFile(t, path, []string{assistantLine, userLine})
	got := extractFirstPrompt(path)
	if got != "actual user" {
		t.Errorf("extractFirstPrompt = %q, want actual user", got)
	}
}

// ---------------------------------------------------------------------------
// RecentSessions
// ---------------------------------------------------------------------------

// makeWorkspace creates a real directory that resolveWorkspaceByParts can find,
// and the matching encoded project dir inside claudeDir.
func makeWorkspace(t *testing.T) (claudeDir, workspace, encodedDir string) {
	t.Helper()
	claudeDir = makeClaudeDir(t)
	// Create the "workspace" as a subdirectory of TempDir so the path exists.
	workspace = filepath.Join(t.TempDir(), "myproject")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	// Encode: replace "/" with "-"
	encoded := workspace
	for i := 0; i < len(encoded); i++ {
		if encoded[i] == '/' {
			encoded = encoded[:i] + "-" + encoded[i+1:]
		}
	}
	encodedDir = encoded // Already has leading "-" because path starts with "/"
	// Clear DFS cache for this key
	dfsPathCache.Delete(encodedDir)
	t.Cleanup(func() { dfsPathCache.Delete(encodedDir) })
	return
}

func TestRecentSessions_EmptyDir(t *testing.T) {
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	got := RecentSessions(claudeDir, 10, 7*24*time.Hour, nil)
	if len(got) != 0 {
		t.Errorf("expected empty sessions from empty dir, got %d", len(got))
	}
}

func TestRecentSessions_EmptyClaudeDir(t *testing.T) {
	t.Parallel()
	got := RecentSessions("", 10, 7*24*time.Hour, nil)
	if got != nil {
		t.Errorf("expected nil for empty claudeDir, got %v", got)
	}
}

func TestRecentSessions_FallbackFromJSONL(t *testing.T) {
	t.Parallel()
	claudeDir, workspace, encodedDir := makeWorkspace(t)

	projDir := filepath.Join(claudeDir, "projects", encodedDir)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sid := "ffffffff-0001-0001-0001-000000000001"
	jsonlPath := filepath.Join(projDir, sid+".jsonl")
	msg, _ := json.Marshal(struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{Role: "user", Content: "test prompt"})
	line := fmt.Sprintf(`{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":%s}`, string(msg))
	writeJSONLFile(t, jsonlPath, []string{line})

	dirFilesCache.Delete(projDir)
	t.Cleanup(func() { dirFilesCache.Delete(projDir) })

	got := RecentSessions(claudeDir, 10, 365*24*time.Hour, nil)
	if len(got) == 0 {
		t.Fatal("expected at least one session")
	}
	found := false
	for _, s := range got {
		if s.SessionID == sid {
			found = true
			if s.Workspace != workspace {
				t.Errorf("workspace = %q, want %q", s.Workspace, workspace)
			}
			if s.LastPrompt != "test prompt" {
				t.Errorf("LastPrompt = %q, want test prompt", s.LastPrompt)
			}
		}
	}
	if !found {
		t.Errorf("session %q not found in results: %+v", sid, got)
	}
}

func TestRecentSessions_WithSessionsIndex(t *testing.T) {
	t.Parallel()
	claudeDir, workspace, encodedDir := makeWorkspace(t)

	projDir := filepath.Join(claudeDir, "projects", encodedDir)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sid := "ffffffff-0001-0001-0001-000000000002"
	if err := os.WriteFile(filepath.Join(projDir, sid+".jsonl"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	writeSessionsIndex(t, projDir, sessionsIndex{
		OriginalPath: workspace,
		Entries: []sessionsIndexEntry{
			{SessionID: sid, Summary: "indexed summary", FirstPrompt: "indexed prompt"},
		},
	})

	dirFilesCache.Delete(projDir)
	t.Cleanup(func() { dirFilesCache.Delete(projDir) })

	got := RecentSessions(claudeDir, 10, 365*24*time.Hour, nil)
	if len(got) == 0 {
		t.Fatal("expected at least one session")
	}
	found := false
	for _, s := range got {
		if s.SessionID == sid {
			found = true
			if s.Summary != "indexed summary" {
				t.Errorf("summary = %q, want indexed summary", s.Summary)
			}
			if s.LastPrompt != "indexed prompt" {
				t.Errorf("LastPrompt = %q, want indexed prompt", s.LastPrompt)
			}
		}
	}
	if !found {
		t.Errorf("session %q not found in results %+v", sid, got)
	}
}

func TestRecentSessions_Limit(t *testing.T) {
	t.Parallel()
	claudeDir, workspace, encodedDir := makeWorkspace(t)

	projDir := filepath.Join(claudeDir, "projects", encodedDir)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a sessions-index with workspace so resolution works via OriginalPath
	sids := []string{
		"ffffffff-0001-0001-0001-000000000011",
		"ffffffff-0001-0001-0001-000000000012",
		"ffffffff-0001-0001-0001-000000000013",
	}

	var entries []sessionsIndexEntry
	for i, sid := range sids {
		time.Sleep(5 * time.Millisecond) // distinct mtimes
		if err := os.WriteFile(filepath.Join(projDir, sid+".jsonl"), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, sessionsIndexEntry{
			SessionID: sid,
			Summary:   fmt.Sprintf("summary %d", i),
		})
	}
	writeSessionsIndex(t, projDir, sessionsIndex{OriginalPath: workspace, Entries: entries})

	dirFilesCache.Delete(projDir)
	t.Cleanup(func() { dirFilesCache.Delete(projDir) })

	got := RecentSessions(claudeDir, 2, 365*24*time.Hour, nil)
	if len(got) != 2 {
		t.Errorf("expected 2 sessions (limit=2), got %d", len(got))
	}
}

func TestRecentSessions_ExcludeByID(t *testing.T) {
	t.Parallel()
	claudeDir, workspace, encodedDir := makeWorkspace(t)

	projDir := filepath.Join(claudeDir, "projects", encodedDir)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sid := "ffffffff-0001-0001-0001-000000000021"
	if err := os.WriteFile(filepath.Join(projDir, sid+".jsonl"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSessionsIndex(t, projDir, sessionsIndex{
		OriginalPath: workspace,
		Entries:      []sessionsIndexEntry{{SessionID: sid, Summary: "exclude me"}},
	})

	dirFilesCache.Delete(projDir)
	t.Cleanup(func() { dirFilesCache.Delete(projDir) })

	got := RecentSessions(claudeDir, 10, 365*24*time.Hour, map[string]bool{sid: true})
	for _, s := range got {
		if s.SessionID == sid {
			t.Errorf("excluded session %q appeared in results", sid)
		}
	}
}

func TestRecentSessions_SkipsHiddenProjectDirs(t *testing.T) {
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	// A dir name containing "--" should be skipped (hidden path pattern)
	hiddenDir := filepath.Join(claudeDir, "projects", "-home--hidden-project")
	if err := os.MkdirAll(hiddenDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "11111111-0001-0001-0001-000000000001"
	if err := os.WriteFile(filepath.Join(hiddenDir, sid+".jsonl"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := RecentSessions(claudeDir, 10, 365*24*time.Hour, nil)
	for _, s := range got {
		if s.SessionID == sid {
			t.Errorf("session from hidden dir should have been skipped, but appeared: %+v", s)
		}
	}
}

func TestRecentSessions_MaxAge(t *testing.T) {
	t.Parallel()
	claudeDir, workspace, encodedDir := makeWorkspace(t)

	projDir := filepath.Join(claudeDir, "projects", encodedDir)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sid := "ffffffff-0001-0001-0001-000000000031"
	if err := os.WriteFile(filepath.Join(projDir, sid+".jsonl"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSessionsIndex(t, projDir, sessionsIndex{
		OriginalPath: workspace,
		Entries:      []sessionsIndexEntry{{SessionID: sid, Summary: "old session"}},
	})

	dirFilesCache.Delete(projDir)
	t.Cleanup(func() { dirFilesCache.Delete(projDir) })

	// Use a tiny maxAge (1ns) so the freshly written file is "too old"
	got := RecentSessions(claudeDir, 10, time.Nanosecond, nil)
	for _, s := range got {
		if s.SessionID == sid {
			t.Errorf("session should be filtered by maxAge, but appeared: %+v", s)
		}
	}
}

func TestRecentSessions_SortedByLastActive(t *testing.T) {
	t.Parallel()
	claudeDir, workspace, encodedDir := makeWorkspace(t)

	projDir := filepath.Join(claudeDir, "projects", encodedDir)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sid1 := "ffffffff-0001-0001-0001-000000000041"
	sid2 := "ffffffff-0001-0001-0001-000000000042"

	if err := os.WriteFile(filepath.Join(projDir, sid1+".jsonl"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // ensure distinct mtimes
	if err := os.WriteFile(filepath.Join(projDir, sid2+".jsonl"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	writeSessionsIndex(t, projDir, sessionsIndex{
		OriginalPath: workspace,
		Entries: []sessionsIndexEntry{
			{SessionID: sid1, Summary: "older"},
			{SessionID: sid2, Summary: "newer"},
		},
	})

	dirFilesCache.Delete(projDir)
	t.Cleanup(func() { dirFilesCache.Delete(projDir) })

	got := RecentSessions(claudeDir, 10, 365*24*time.Hour, nil)
	if len(got) < 2 {
		t.Fatalf("expected at least 2 sessions, got %d", len(got))
	}
	// Newest first
	if got[0].SessionID != sid2 {
		t.Errorf("first session = %q, want %q (newest first)", got[0].SessionID, sid2)
	}
	if got[1].SessionID != sid1 {
		t.Errorf("second session = %q, want %q", got[1].SessionID, sid1)
	}
}

func TestRecentSessions_ZeroLimit(t *testing.T) {
	t.Parallel()
	claudeDir, workspace, encodedDir := makeWorkspace(t)

	projDir := filepath.Join(claudeDir, "projects", encodedDir)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sids := []string{
		"ffffffff-0001-0001-0001-000000000051",
		"ffffffff-0001-0001-0001-000000000052",
		"ffffffff-0001-0001-0001-000000000053",
	}
	var entries []sessionsIndexEntry
	for _, sid := range sids {
		if err := os.WriteFile(filepath.Join(projDir, sid+".jsonl"), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, sessionsIndexEntry{SessionID: sid, Summary: "s"})
	}
	writeSessionsIndex(t, projDir, sessionsIndex{OriginalPath: workspace, Entries: entries})

	dirFilesCache.Delete(projDir)
	t.Cleanup(func() { dirFilesCache.Delete(projDir) })

	// limit=0 means "return all"
	got := RecentSessions(claudeDir, 0, 365*24*time.Hour, nil)
	if len(got) < 3 {
		t.Errorf("limit=0 should return all sessions, got %d", len(got))
	}
}
