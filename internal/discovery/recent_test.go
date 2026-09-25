package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
)

// ---------------------------------------------------------------------------
// resolveWorkspaceByParts
// ---------------------------------------------------------------------------

func TestResolveWorkspaceByParts_RealDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	base := filepath.Join(root, "work", "proj")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	encoded := slugUnder(t, root, base)

	if got := resolveWorkspaceUnder(root, encoded); got != base {
		t.Errorf("resolveWorkspaceUnder(%q) = %q, want %q", encoded, got, base)
	}
}

// TestResolveWorkspaceUnder_StaysInsideRoot is #2744's containment property:
// whatever a rooted resolution returns lies inside the root, so a test that
// injects t.TempDir() cannot be resolving — or walking — the host's real tree.
func TestResolveWorkspaceUnder_StaysInsideRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, rel := range []string{
		"a",
		filepath.Join("a", "b"),
		filepath.Join("repo", ".claude", "worktrees", "wt"), // pass 2 (dot segment)
		filepath.Join("with_under", "x"),                    // pass 2 ("_" segment)
	} {
		want := filepath.Join(root, rel)
		if err := os.MkdirAll(want, 0o755); err != nil {
			t.Fatal(err)
		}
		got := resolveWorkspaceUnder(root, slugUnder(t, root, want))
		if got != want {
			t.Errorf("rel %q resolved to %q, want %q", rel, got, want)
			continue
		}
		if r, err := filepath.Rel(root, got); err != nil || strings.HasPrefix(r, "..") {
			t.Errorf("rel %q resolved outside the root: %q", rel, got)
		}
	}
	// A name that only exists on the HOST (e.g. "/usr" is on every Unix box)
	// must not resolve under an injected root: the walk never leaves it.
	if got := resolveWorkspaceUnder(root, "-usr"); got != "" {
		t.Errorf(`"-usr" resolved to %q under an empty root; the resolver walked the host tree`, got)
	}
}

func TestResolveWorkspaceByParts_Cache(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	base := filepath.Join(root, "cached")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	encoded := slugUnder(t, root, base)

	got1 := resolveWorkspaceUnder(root, encoded)
	// Remove the dir: only a cached answer can still return the path.
	if err := os.RemoveAll(base); err != nil {
		t.Fatal(err)
	}
	if got2 := resolveWorkspaceUnder(root, encoded); got1 != base || got2 != base {
		t.Errorf("cache not served: first=%q second=%q, want %q both times", got1, got2, base)
	}
}

// TestResolveWorkspaceUnder_CacheIsScopedToRoot: the same encoded name under
// two roots must decode to two paths. An unscoped cache would hand the second
// root the first root's answer — and a test under t.TempDir() could poison the
// production "/" entry.
func TestResolveWorkspaceUnder_CacheIsScopedToRoot(t *testing.T) {
	t.Parallel()
	rootA, rootB := t.TempDir(), t.TempDir()
	for _, r := range []string{rootA, rootB} {
		if err := os.MkdirAll(filepath.Join(r, "same"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := resolveWorkspaceUnder(rootA, "-same"); got != filepath.Join(rootA, "same") {
		t.Fatalf("rootA: %q", got)
	}
	if got := resolveWorkspaceUnder(rootB, "-same"); got != filepath.Join(rootB, "same") {
		t.Errorf("rootB got %q — the cache leaked rootA's answer across roots", got)
	}
}

func TestResolveWorkspaceByParts_NonexistentPath(t *testing.T) {
	t.Parallel()
	if got := resolveWorkspaceUnder(t.TempDir(), "-nonexistent-path-that-cannot-exist-xyz987"); got != "" {
		t.Errorf("expected empty for nonexistent path, got %q", got)
	}
}

// TestResolveWorkspaceByParts_NegativeResultNotCached is a regression test for
// #1994: a workspace dir absent during one scan (unmounted drive, worktree
// mid-rebuild) must still resolve once it reappears, rather than being
// permanently cached as unresolvable for the process lifetime.
func TestResolveWorkspaceByParts_NegativeResultNotCached(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "reappearing-project")
	encoded := slugUnder(t, root, dir)
	key := dfsCacheKey(root, encoded)

	// Dir absent: resolves to "" and must NOT be cached.
	if got := resolveWorkspaceUnder(root, encoded); got != "" {
		t.Fatalf("expected empty for absent dir, got %q", got)
	}
	if _, ok := dfsPathCache.Load(key); ok {
		t.Fatal("negative result must not be cached (#1994)")
	}

	// Dir reappears: must now resolve instead of returning the stale "".
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolveWorkspaceUnder(root, encoded); got != dir {
		t.Errorf("after dir reappeared, resolveWorkspaceUnder(%q) = %q, want %q", encoded, got, dir)
	}
	if _, ok := dfsPathCache.Load(key); !ok {
		t.Error("positive result should be cached")
	}
}

// TestResolveWorkspaceByParts_ProductionRootIsSlash pins the one thing every
// other test here now bypasses: the production entry point resolves from "/".
// "/usr" exists on every Unix host and "-usr" resolves on pass 1 with a single
// Stat — no ReadDir, so no walk into anything the host has mounted.
func TestResolveWorkspaceByParts_ProductionRootIsSlash(t *testing.T) {
	t.Parallel()
	if fi, err := os.Stat("/usr"); err != nil || !fi.IsDir() {
		t.Skip("no /usr on this host")
	}
	if got := resolveWorkspaceByParts("-usr"); got != "/usr" {
		t.Errorf(`resolveWorkspaceByParts("-usr") = %q, want "/usr" — production must resolve from the real root`, got)
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
			if got := resolveWorkspaceUnder(t.TempDir(), tc.input); got != "" {
				t.Errorf("resolveWorkspaceUnder(%q) = %q, want empty", tc.input, got)
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

// makeWorkspace creates a real workspace directory below a fresh resolution
// root and returns the root, a ~/.claude dir, the workspace, and the project
// dir name that encodes it relative to root. Resolving under root instead of
// "/" keeps the walk inside t.TempDir(): from "/" the second pass can descend
// into whatever the host has mounted (#2744).
func makeWorkspace(t *testing.T) (root, claudeDir, workspace, encodedDir string) {
	t.Helper()
	claudeDir = makeClaudeDir(t)
	root = t.TempDir()
	workspace = filepath.Join(root, "myproject")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	encodedDir = slugUnder(t, root, workspace)
	return
}

// slugUnder encodes path the way Claude names its project directory, as if root
// were "/": the name a resolver rooted at root decodes back to path.
func slugUnder(t *testing.T, root, path string) string {
	t.Helper()
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("slugUnder: %q is not below %q", path, root)
	}
	return claudefs.ProjectSlug("/" + filepath.ToSlash(rel))
}

func TestRecentSessions_EmptyDir(t *testing.T) {
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	got := recentSessionsUnder(context.Background(), t.TempDir(), claudeDir, 10, 7*24*time.Hour, nil, nil)
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
	root, claudeDir, workspace, encodedDir := makeWorkspace(t)

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

	got := recentSessionsUnder(context.Background(), root, claudeDir, 10, 365*24*time.Hour, nil, nil)
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
	root, claudeDir, workspace, encodedDir := makeWorkspace(t)

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

	got := recentSessionsUnder(context.Background(), root, claudeDir, 10, 365*24*time.Hour, nil, nil)
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
	root, claudeDir, workspace, encodedDir := makeWorkspace(t)

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

	got := recentSessionsUnder(context.Background(), root, claudeDir, 2, 365*24*time.Hour, nil, nil)
	if len(got) != 2 {
		t.Errorf("expected 2 sessions (limit=2), got %d", len(got))
	}
}

func TestRecentSessions_ExcludeByID(t *testing.T) {
	t.Parallel()
	root, claudeDir, workspace, encodedDir := makeWorkspace(t)

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

	got := recentSessionsUnder(context.Background(), root, claudeDir, 10, 365*24*time.Hour, map[string]bool{sid: true}, nil)
	for _, s := range got {
		if s.SessionID == sid {
			t.Errorf("excluded session %q appeared in results", sid)
		}
	}
}

// TestRecentSessions_SkipsUnresolvableProjectDirs pins the `workspace == ""`
// gate: a project dir whose encoded name decodes to no real path yields no
// session at all, rather than one with an empty workspace that the sidebar
// would render without a label.
//
// It used to be called SkipsHiddenProjectDirs and used "-home--hidden-project",
// which was wrong twice over. It never exercised the hidden-path logic — the
// name is unresolvable on any host, so the session was dropped one layer
// earlier, and the test still passed with isHiddenToolWorkspace neutered (that
// behaviour is covered by StillSkipsToolHiddenDirs,
// RelativeHiddenWorkspaceStillSkipped and TestIsHiddenToolWorkspace). And the
// "home" segment matched the real /home, which on macOS is an autofs trigger:
// resolveByDirScan descended into it and paid ~2s per ReadDir, in a unit test.
//
// So the first segment below must not be the encoding of any real top-level
// directory. The fallback scan then re-encodes the children of "/" , matches
// none, and returns without descending anywhere.
func TestRecentSessions_SkipsUnresolvableProjectDirs(t *testing.T) {
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	// An empty resolution root makes the name unresolvable by construction.
	// Resolving from "/" instead needed a host-side premise guard (skip if a
	// real /zzz ever existed) — exactly the host dependency #2744 removes.
	root := t.TempDir()
	const encodedDir = "-zzz-naozhi-no-such-workspace-project"
	unresolvable := filepath.Join(claudeDir, "projects", encodedDir)
	if err := os.MkdirAll(unresolvable, 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "11111111-0001-0001-0001-000000000001"
	if err := os.WriteFile(filepath.Join(unresolvable, sid+".jsonl"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := recentSessionsUnder(context.Background(), root, claudeDir, 10, 365*24*time.Hour, nil, nil)
	for _, s := range got {
		if s.SessionID == sid {
			t.Errorf("session from an unresolvable project dir should have been skipped, but appeared: %+v", s)
		}
	}
}

func TestRecentSessions_MaxAge(t *testing.T) {
	t.Parallel()
	root, claudeDir, workspace, encodedDir := makeWorkspace(t)

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
	got := recentSessionsUnder(context.Background(), root, claudeDir, 10, time.Minute, nil, nil)
	for _, s := range got {
		if s.SessionID == sid {
			t.Errorf("session should be filtered by maxAge, but appeared: %+v", s)
		}
	}
}

func TestRecentSessions_SortedByLastActive(t *testing.T) {
	t.Parallel()
	root, claudeDir, workspace, encodedDir := makeWorkspace(t)

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

	got := recentSessionsUnder(context.Background(), root, claudeDir, 10, 365*24*time.Hour, nil, nil)
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
	root, claudeDir, workspace, encodedDir := makeWorkspace(t)

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
	got := recentSessionsUnder(context.Background(), root, claudeDir, 0, 365*24*time.Hour, nil, nil)
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
	root, claudeDir, workspace, encodedDir := makeWorkspace(t)

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
	got := recentSessionsUnder(context.Background(), root, claudeDir, 10, 365*24*time.Hour, nil, filter)
	for _, s := range got {
		if s.SessionID == sid {
			t.Errorf("workspace-blacklisted session leaked into result: %+v", s)
		}
	}
}

func TestRecentSessions_FilterSkipsSessionID(t *testing.T) {
	t.Parallel()
	root, claudeDir, workspace, encodedDir := makeWorkspace(t)

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
	got := recentSessionsUnder(context.Background(), root, claudeDir, 10, 365*24*time.Hour, nil, filter)

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
	root, claudeDir, workspace, encodedDir := makeWorkspace(t)

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

	got := recentSessionsUnder(context.Background(), root, claudeDir, 10, 365*24*time.Hour, nil, nil)
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

// TestRecentSessionsCtx_CancelledReturnsEarly guards PERF-009 (#2134): an
// already-cancelled context must short-circuit the FS walk so a slow/hung
// home cannot pin the singleflight leader. The result is best-effort
// (empty here because the very first iteration sees ctx.Err()).
func TestRecentSessionsCtx_CancelledReturnsEarly(t *testing.T) {
	t.Parallel()
	root, claudeDir, workspace, encodedDir := makeWorkspace(t)

	projDir := filepath.Join(claudeDir, "projects", encodedDir)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "dddddddd-0001-0001-0001-000000000001"
	if err := os.WriteFile(filepath.Join(projDir, sid+".jsonl"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSessionsIndex(t, projDir, sessionsIndex{
		OriginalPath: workspace,
		Entries:      []sessionsIndexEntry{{SessionID: sid, Summary: "ok"}},
	})
	dirFilesCache.Delete(projDir)
	t.Cleanup(func() { dirFilesCache.Delete(projDir) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the walk starts

	got := recentSessionsUnder(ctx, root, claudeDir, 10, 365*24*time.Hour, nil, nil)
	if len(got) != 0 {
		t.Errorf("cancelled context must yield empty (early-return) result, got %d sessions", len(got))
	}
}

// TestRecentSessionsCtx_BackgroundCtxEquivalent confirms a live context
// leaves behaviour identical to the legacy RecentSessions path.
func TestRecentSessionsCtx_BackgroundCtxEquivalent(t *testing.T) {
	t.Parallel()
	root, claudeDir, workspace, encodedDir := makeWorkspace(t)

	projDir := filepath.Join(claudeDir, "projects", encodedDir)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "dddddddd-0001-0001-0001-000000000002"
	if err := os.WriteFile(filepath.Join(projDir, sid+".jsonl"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSessionsIndex(t, projDir, sessionsIndex{
		OriginalPath: workspace,
		Entries:      []sessionsIndexEntry{{SessionID: sid, Summary: "ok"}},
	})
	dirFilesCache.Delete(projDir)
	t.Cleanup(func() { dirFilesCache.Delete(projDir) })

	got := recentSessionsUnder(context.Background(), root, claudeDir, 10, 365*24*time.Hour, nil, nil)
	var saw bool
	for _, s := range got {
		if s.SessionID == sid {
			saw = true
		}
	}
	if !saw {
		t.Errorf("background ctx must behave like RecentSessions; session missing: %s", sid)
	}
}
