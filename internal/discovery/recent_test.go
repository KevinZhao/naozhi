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

	// Regression (#1994): a negative ("") result must NOT be cached, so a
	// directory that is transiently absent during one scan stays unresolvable
	// without poisoning future scans.
	if _, ok := dfsPathCache.Load(encoded); ok {
		t.Errorf("negative result was cached for %q; should be skipped", encoded)
	}
}

// TestResolveWorkspaceByParts_NegativeNotCached verifies that a directory which
// is absent on the first call (returns "") becomes resolvable once it reappears
// on disk, instead of being permanently stuck at "" by a cached negative result
// (regression for #1994: removable/network drive remount, git worktree
// remove+recreate, mid-checkout transient absence).
func TestResolveWorkspaceByParts_NegativeNotCached(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	target := filepath.Join(parent, "reappear")
	encoded := "-" + strings.ReplaceAll(target[1:], "/", "-")
	dfsPathCache.Delete(encoded)
	t.Cleanup(func() { dfsPathCache.Delete(encoded) })

	// First call: target does not exist yet → unresolvable.
	if got := resolveWorkspaceByParts(encoded); got != "" {
		t.Fatalf("expected empty before dir exists, got %q", got)
	}
	if _, ok := dfsPathCache.Load(encoded); ok {
		t.Fatalf("negative result was cached; should not be")
	}

	// Directory reappears (e.g. drive remounted / worktree recreated).
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Second call must now resolve it, proving the negative result was not cached.
	if got := resolveWorkspaceByParts(encoded); got != target {
		t.Errorf("after dir reappeared: got %q, want %q", got, target)
	}
}

// TestResolveWorkspaceByParts_PositiveCached verifies successful resolutions are
// still cached and served from the cache on subsequent calls.
func TestResolveWorkspaceByParts_PositiveCached(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	encoded := "-" + strings.ReplaceAll(base[1:], "/", "-")
	dfsPathCache.Delete(encoded)
	t.Cleanup(func() { dfsPathCache.Delete(encoded) })

	if got := resolveWorkspaceByParts(encoded); got != base {
		t.Fatalf("first resolution: got %q, want %q", got, base)
	}
	v, ok := dfsPathCache.Load(encoded)
	if !ok || v.(string) != base {
		t.Fatalf("positive result not cached: ok=%v v=%v", ok, v)
	}
	if got := resolveWorkspaceByParts(encoded); got != base {
		t.Errorf("cached resolution: got %q, want %q", got, base)
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

// TestCachedJSONLByID_ReusedAcrossCalls pins R247-PERF-19: the sessionID→mtime
// map is built once at cache fill time and reused on subsequent calls (same
// directory mtime). Identity equality (==) on the map header is the cleanest
// way to confirm the second call did not allocate a fresh map.
func TestCachedJSONLByID_ReusedAcrossCalls(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "aaaaaaaa-0002-0002-0002-000000000001"
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirFilesCache.Delete(dir)
	t.Cleanup(func() { dirFilesCache.Delete(dir) })

	m1 := cachedJSONLByID(dir)
	m2 := cachedJSONLByID(dir)

	if m1 == nil || m2 == nil {
		t.Fatalf("expected non-nil maps, got m1=%v m2=%v", m1, m2)
	}
	if _, ok := m1[sid]; !ok {
		t.Errorf("byID missing session %q: %v", sid, m1)
	}
	// Same map header — confirms no per-call rebuild.
	// (reflect.ValueOf().Pointer() returns the underlying hashmap pointer.)
	if fmt.Sprintf("%p", m1) != fmt.Sprintf("%p", m2) {
		t.Errorf("cachedJSONLByID rebuilt map per call: %p vs %p", m1, m2)
	}
}

// TestCachedJSONLByID_EmptyDirReturnsNilMap documents the empty-dir contract:
// no .jsonl entries means no map allocation at all (saves a 0-cap map on
// every cache-miss for fresh project dirs).
func TestCachedJSONLByID_EmptyDirReturnsNilMap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dirFilesCache.Delete(dir)
	t.Cleanup(func() { dirFilesCache.Delete(dir) })

	if got := cachedJSONLByID(dir); got != nil {
		t.Errorf("cachedJSONLByID(empty dir) = %v, want nil", got)
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
	got := RecentSessions(claudeDir, 10, 7*24*time.Hour, nil, nil)
	if len(got) != 0 {
		t.Errorf("expected empty sessions from empty dir, got %d", len(got))
	}
}

func TestRecentSessions_EmptyClaudeDir(t *testing.T) {
	t.Parallel()
	got := RecentSessions("", 10, 7*24*time.Hour, nil, nil)
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

	got := RecentSessions(claudeDir, 10, 365*24*time.Hour, nil, nil)
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

	got := RecentSessions(claudeDir, 10, 365*24*time.Hour, nil, nil)
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

	got := RecentSessions(claudeDir, 2, 365*24*time.Hour, nil, nil)
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

	got := RecentSessions(claudeDir, 10, 365*24*time.Hour, map[string]bool{sid: true}, nil)
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

	got := RecentSessions(claudeDir, 10, 365*24*time.Hour, nil, nil)
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
	jsonlPath := filepath.Join(projDir, sid+".jsonl")
	if err := os.WriteFile(jsonlPath, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Force the JSONL mtime an hour into the past so the maxAge filter has
	// unambiguous comparison room. Without this, CI runners where the test
	// wrote the file and called RecentSessions within the same millisecond
	// observed mtime == cutoff and the "<" comparison failed to exclude
	// the "old" session — the previous 1ns maxAge tried to express the
	// intent but the filter rounds to ms, so 1ns and 0ns were identical.
	oldMtime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(jsonlPath, oldMtime, oldMtime); err != nil {
		t.Fatal(err)
	}
	writeSessionsIndex(t, projDir, sessionsIndex{
		OriginalPath: workspace,
		Entries:      []sessionsIndexEntry{{SessionID: sid, Summary: "old session"}},
	})

	dirFilesCache.Delete(projDir)
	t.Cleanup(func() { dirFilesCache.Delete(projDir) })

	// maxAge = 1 minute; the file is 1 hour old so it must be filtered out.
	got := RecentSessions(claudeDir, 10, time.Minute, nil, nil)
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

	got := RecentSessions(claudeDir, 10, 365*24*time.Hour, nil, nil)
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
	got := RecentSessions(claudeDir, 0, 365*24*time.Hour, nil, nil)
	if len(got) < 3 {
		t.Errorf("limit=0 should return all sessions, got %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// RecentSessionsFilter (R245-ARCH)
// ---------------------------------------------------------------------------

// stubFilter is a table-friendly RecentSessionsFilter for unit tests.
type stubFilter struct {
	skipWorkspaces map[string]bool
	skipSessionIDs map[string]bool
}

func (s stubFilter) SkipWorkspace(ws string) bool  { return s.skipWorkspaces[ws] }
func (s stubFilter) SkipSessionID(sid string) bool { return s.skipSessionIDs[sid] }

func TestRecentSessions_FilterSkipsWorkspace(t *testing.T) {
	t.Parallel()
	claudeDir, workspace, encodedDir := makeWorkspace(t)

	projDir := filepath.Join(claudeDir, "projects", encodedDir)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "aaaaaaaa-0001-0001-0001-000000000001"
	if err := os.WriteFile(filepath.Join(projDir, sid+".jsonl"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSessionsIndex(t, projDir, sessionsIndex{
		OriginalPath: workspace,
		Entries:      []sessionsIndexEntry{{SessionID: sid, Summary: "should be hidden"}},
	})
	dirFilesCache.Delete(projDir)
	t.Cleanup(func() { dirFilesCache.Delete(projDir) })

	filter := stubFilter{skipWorkspaces: map[string]bool{workspace: true}}
	got := RecentSessions(claudeDir, 10, 365*24*time.Hour, nil, filter)
	for _, s := range got {
		if s.SessionID == sid {
			t.Errorf("workspace-blacklisted session leaked into result: %+v", s)
		}
	}
}

func TestRecentSessions_FilterSkipsSessionID(t *testing.T) {
	t.Parallel()
	claudeDir, workspace, encodedDir := makeWorkspace(t)

	projDir := filepath.Join(claudeDir, "projects", encodedDir)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	visibleSID := "bbbbbbbb-0001-0001-0001-000000000002"
	hiddenSID := "bbbbbbbb-0001-0001-0001-000000000003"
	for _, sid := range []string{visibleSID, hiddenSID} {
		if err := os.WriteFile(filepath.Join(projDir, sid+".jsonl"), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeSessionsIndex(t, projDir, sessionsIndex{
		OriginalPath: workspace,
		Entries: []sessionsIndexEntry{
			{SessionID: visibleSID, Summary: "show"},
			{SessionID: hiddenSID, Summary: "hide"},
		},
	})
	dirFilesCache.Delete(projDir)
	t.Cleanup(func() { dirFilesCache.Delete(projDir) })

	filter := stubFilter{skipSessionIDs: map[string]bool{hiddenSID: true}}
	got := RecentSessions(claudeDir, 10, 365*24*time.Hour, nil, filter)

	var sawVisible, sawHidden bool
	for _, s := range got {
		if s.SessionID == visibleSID {
			sawVisible = true
		}
		if s.SessionID == hiddenSID {
			sawHidden = true
		}
	}
	if !sawVisible {
		t.Errorf("non-blacklisted session was filtered: visibleSID=%s", visibleSID)
	}
	if sawHidden {
		t.Errorf("blacklisted session leaked into result: hiddenSID=%s", hiddenSID)
	}
}

// TestRecentSessions_NilFilterIsNoop guards against future "if filter == nil
// { return nil }" defensive code that would silently empty the history list.
func TestRecentSessions_NilFilterIsNoop(t *testing.T) {
	t.Parallel()
	claudeDir, workspace, encodedDir := makeWorkspace(t)

	projDir := filepath.Join(claudeDir, "projects", encodedDir)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "cccccccc-0001-0001-0001-000000000004"
	if err := os.WriteFile(filepath.Join(projDir, sid+".jsonl"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSessionsIndex(t, projDir, sessionsIndex{
		OriginalPath: workspace,
		Entries:      []sessionsIndexEntry{{SessionID: sid, Summary: "ok"}},
	})
	dirFilesCache.Delete(projDir)
	t.Cleanup(func() { dirFilesCache.Delete(projDir) })

	got := RecentSessions(claudeDir, 10, 365*24*time.Hour, nil, nil)
	var saw bool
	for _, s := range got {
		if s.SessionID == sid {
			saw = true
		}
	}
	if !saw {
		t.Errorf("nil filter must behave like no-op; session missing: %s", sid)
	}
}
