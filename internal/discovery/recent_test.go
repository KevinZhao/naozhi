package discovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
)

// ---------------------------------------------------------------------------
// resolveWorkspaceUnder
// ---------------------------------------------------------------------------

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

func TestResolveWorkspaceUnder_Cache(t *testing.T) {
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

// TestResolveWorkspaceUnder_NegativeResultNotCached is a regression test for
// #1994: a workspace dir absent during one scan (unmounted drive, worktree
// mid-rebuild) must still resolve once it reappears, rather than being
// permanently cached as unresolvable for the process lifetime.
func TestResolveWorkspaceUnder_NegativeResultNotCached(t *testing.T) {
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

// TestResolveWorkspaceUnder_ProductionRootIsSlash pins the one thing every
// other test here now bypasses: the production entry point resolves from "/".
// "/usr" exists on every Unix host and "-usr" resolves on pass 1 with a single
// Stat — no ReadDir, so no walk into anything the host has mounted.
func TestResolveWorkspaceUnder_ProductionRootIsSlash(t *testing.T) {
	t.Parallel()
	if fi, err := os.Stat("/usr"); err != nil || !fi.IsDir() {
		t.Skip("no /usr on this host")
	}
	if got := resolveWorkspaceUnder(fsRoot, "-usr"); got != "/usr" {
		t.Errorf(`resolveWorkspaceUnder(fsRoot, "-usr") = %q, want "/usr" — production must resolve from the real root`, got)
	}
}

func TestResolveWorkspaceUnder_Unresolvable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"no leading dash", "home-user-project"},
		{"nonexistent path", "-nonexistent-path-that-cannot-exist-xyz987"},
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
// extractFirstPrompt
// ---------------------------------------------------------------------------

// TestExtractFirstPrompt: the first user line's text, skipping what comes
// before it; nothing for a missing file.
func TestExtractFirstPrompt(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "multi.jsonl")
	writeJSONLFile(t, path, []string{
		assistantJSONLLine("assistant text"),
		userJSONLLine("user", "first"),
		userJSONLLine("user", "second"),
	})
	if got := extractFirstPrompt(path); got != "first" {
		t.Errorf("extractFirstPrompt = %q, want first", got)
	}
	if got := extractFirstPrompt("/nonexistent/session.jsonl"); got != "" {
		t.Errorf("expected empty for nonexistent file, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// recentFromJSONLFiles / recentFromParsedIndex
// ---------------------------------------------------------------------------

// writeJSONLs writes a non-empty <sid>.jsonl per id into dir and drops dir's
// listing from the cache.
func writeJSONLs(t *testing.T, dir string, sids ...string) {
	t.Helper()
	for _, sid := range sids {
		if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dirFilesCache.Delete(dir)
	t.Cleanup(func() { dirFilesCache.Delete(dir) })
}

// sessionIDs lists the results' session IDs in order.
func sessionIDs(got []RecentSession) []string {
	var ids []string
	for _, s := range got {
		ids = append(ids, s.SessionID)
	}
	return ids
}

// TestRecentFromJSONLFiles: one result per .jsonl named by a valid session ID
// and not excluded, carrying the workspace.
func TestRecentFromJSONLFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const workspace = "/tmp/test-workspace"
	keep, excluded := "cccccccc-0001-0001-0001-000000000001", "cccccccc-0001-0001-0001-000000000002"
	writeJSONLs(t, dir, "not-a-uuid", keep, excluded)

	results := recentFromJSONLFiles(dir, workspace, map[string]bool{excluded: true})
	if len(results) != 1 || results[0].SessionID != keep || results[0].Workspace != workspace {
		t.Fatalf("results = %+v, want only %s in %s (invalid UUID and excluded skipped)", results, keep, workspace)
	}
}

// TestRecentFromParsedIndex: an index entry yields a result only when its
// .jsonl exists and it is not excluded; its prompt is the index's FirstPrompt,
// else its Summary.
func TestRecentFromParsedIndex(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const workspace = "/tmp/index-project"
	withPrompt := "dddddddd-0001-0001-0001-000000000001"
	summaryOnly := "dddddddd-0001-0001-0001-000000000002"
	noJSONL := "dddddddd-0001-0001-0001-000000000003"
	excluded := "dddddddd-0001-0001-0001-000000000004"
	writeJSONLs(t, dir, withPrompt, summaryOnly, excluded)

	idx := &sessionsIndex{OriginalPath: workspace, Entries: []sessionsIndexEntry{
		{SessionID: withPrompt, Summary: "index summary", FirstPrompt: "first prompt from index"},
		{SessionID: summaryOnly, Summary: "fallback to summary"},
		{SessionID: noJSONL, Summary: "ghost session"},
		{SessionID: excluded, Summary: "excluded"},
	}}
	results := recentFromParsedIndex(idx, dir, workspace, map[string]bool{excluded: true})
	got := map[string]RecentSession{}
	for _, r := range results {
		got[r.SessionID] = r
	}
	if len(results) != 2 {
		t.Fatalf("results = %v, want %s and %s", sessionIDs(results), withPrompt, summaryOnly)
	}
	if r := got[withPrompt]; r.Summary != "index summary" || r.LastPrompt != "first prompt from index" {
		t.Errorf("with FirstPrompt: summary %q prompt %q", r.Summary, r.LastPrompt)
	}
	if r := got[summaryOnly]; r.LastPrompt != "fallback to summary" {
		t.Errorf("without FirstPrompt: prompt %q, want the summary", r.LastPrompt)
	}
}

// ---------------------------------------------------------------------------
// RecentSessionsCtx
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
	got := RecentSessionsCtx(context.Background(), "", 10, 7*24*time.Hour, nil, nil)
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
	line := userJSONLLine("user", "test prompt")
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

// indexedProject is a workspace whose project dir holds a .jsonl per sid, the
// later ones a second newer each, and a sessions-index naming them.
func indexedProject(t *testing.T, sids ...string) (root, claudeDir, workspace, projDir string) {
	t.Helper()
	root, claudeDir, workspace, encodedDir := makeWorkspace(t)
	projDir = filepath.Join(claudeDir, "projects", encodedDir)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONLs(t, projDir, sids...)
	var entries []sessionsIndexEntry
	for i, sid := range sids {
		mtime := time.Now().Add(time.Duration(i-len(sids)) * time.Second)
		if err := os.Chtimes(filepath.Join(projDir, sid+".jsonl"), mtime, mtime); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, sessionsIndexEntry{SessionID: sid, Summary: "summary " + sid})
	}
	writeSessionsIndex(t, projDir, sessionsIndex{OriginalPath: workspace, Entries: entries})
	return root, claudeDir, workspace, projDir
}

// TestRecentSessions_IndexedProject: sessions come from the index, newest
// first; limit caps them (0 is no cap); excluded IDs, a filter's workspaces
// and session IDs, sessions older than maxAge and a cancelled context
// (PERF-009, #2134: a hung home must not pin the singleflight leader) drop
// them.
func TestRecentSessions_IndexedProject(t *testing.T) {
	t.Parallel()
	const older, newer = "ffffffff-0001-0001-0001-000000000041", "ffffffff-0001-0001-0001-000000000042"
	root, claudeDir, workspace, projDir := indexedProject(t, older, newer)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	year := 365 * 24 * time.Hour
	cases := []struct {
		name    string
		ctx     context.Context
		limit   int
		maxAge  time.Duration
		exclude map[string]bool
		filter  RecentSessionsFilter
		want    []string
	}{
		{"newest first", context.Background(), 10, year, nil, nil, []string{newer, older}},
		{"limit", context.Background(), 1, year, nil, nil, []string{newer}},
		{"zero limit is all", context.Background(), 0, year, nil, nil, []string{newer, older}},
		{"exclude by ID", context.Background(), 10, year, map[string]bool{newer: true}, nil, []string{older}},
		{"filter skips workspace", context.Background(), 10, year, nil, stubFilter{skipWorkspaces: map[string]bool{workspace: true}}, nil},
		{"filter skips session ID", context.Background(), 10, year, nil, stubFilter{skipSessionIDs: map[string]bool{older: true}}, []string{newer}},
		{"cancelled ctx", cancelled, 10, year, nil, nil, nil},
	}
	for _, tc := range cases {
		got := recentSessionsUnder(tc.ctx, root, claudeDir, tc.limit, tc.maxAge, tc.exclude, tc.filter)
		if ids := sessionIDs(got); !slices.Equal(ids, tc.want) {
			t.Errorf("%s: sessions %v, want %v", tc.name, ids, tc.want)
		}
	}
	for _, s := range recentSessionsUnder(context.Background(), root, claudeDir, 10, year, nil, nil) {
		if s.Summary != "summary "+s.SessionID || s.Workspace != workspace {
			t.Errorf("session %s: summary %q workspace %q", s.SessionID, s.Summary, s.Workspace)
		}
	}

	// The filter rounds to ms, so an mtime an hour back leaves no doubt.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(projDir, older+".jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	dirFilesCache.Delete(projDir)
	if ids := sessionIDs(recentSessionsUnder(context.Background(), root, claudeDir, 10, time.Minute, nil, nil)); !slices.Equal(ids, []string{newer}) {
		t.Errorf("maxAge: sessions %v, want only %s", ids, newer)
	}
}

// errAfterCtx is a context whose Err stays nil for the first n calls and then
// reports a deadline, so a test can land the cancellation between the walk
// and the prompt extraction that follows it.
type errAfterCtx struct {
	context.Context
	n int
}

func (c *errAfterCtx) Err() error {
	if c.n > 0 {
		c.n--
		return nil
	}
	return context.DeadlineExceeded
}

// TestRecentSessionsCtx_CancelledDuringExtractionKeepsSessions pins #3141: a
// deadline that hits after the walk found its sessions must stop the prompt
// reads, not drop the sessions. The old loop broke out on the first ctx.Err()
// and returned nothing, which emptied the dashboard's history under load.
func TestRecentSessionsCtx_CancelledDuringExtractionKeepsSessions(t *testing.T) {
	t.Parallel()
	root, claudeDir, _, encodedDir := makeWorkspace(t)

	projDir := filepath.Join(claudeDir, "projects", encodedDir)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := userJSONLLine("user", "a prompt that needs a read")
	sids := []string{
		"dddddddd-0001-0001-0001-000000000003",
		"dddddddd-0001-0001-0001-000000000004",
	}
	for _, sid := range sids {
		writeJSONLFile(t, filepath.Join(projDir, sid+".jsonl"), []string{line})
	}
	dirFilesCache.Delete(projDir)
	t.Cleanup(func() { dirFilesCache.Delete(projDir) })

	// One nil for the walk's single project directory; the deadline then
	// lands on the first extraction check.
	ctx := &errAfterCtx{Context: context.Background(), n: 1}
	got := recentSessionsUnder(ctx, root, claudeDir, 10, 365*24*time.Hour, nil, nil)
	if len(got) != len(sids) {
		t.Fatalf("a deadline during extraction must keep the walked sessions: got %d, want %d (%+v)", len(got), len(sids), got)
	}
	for _, s := range got {
		if s.LastPrompt != "" {
			t.Errorf("session %s: LastPrompt = %q, want empty once extraction is cancelled", s.SessionID, s.LastPrompt)
		}
	}
}

// TestRecentSessions_StaleDirNotResolved: a project directory whose newest
// jsonl is older than maxAge contributes nothing, so the scan must not pay for
// resolving its workspace (most such directories name a deleted worktree, and
// a failed resolution is an uncached DFS on every scan). A jsonl exactly at
// the cutoff still counts, matching the LastActive >= cutoff filter.
func TestRecentSessions_StaleDirNotResolved(t *testing.T) {
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	root := t.TempDir()
	write := func(name, sid string, mtime time.Time) string {
		ws := filepath.Join(root, name)
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		enc := slugUnder(t, root, ws)
		projDir := filepath.Join(claudeDir, "projects", enc)
		if err := os.MkdirAll(projDir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(projDir, sid+".jsonl")
		if err := os.WriteFile(p, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			dirFilesCache.Delete(projDir)
			dfsPathCache.Delete(dfsCacheKey(root, enc))
		})
		return enc
	}
	staleSID := "eeeeeeee-0001-0001-0001-000000000001"
	freshSID := "eeeeeeee-0001-0001-0001-000000000002"
	staleEnc := write("staleproj", staleSID, time.Now().Add(-2*time.Hour))
	freshEnc := write("freshproj", freshSID, time.Now().Add(-time.Minute))

	got := recentSessionsUnder(context.Background(), root, claudeDir, 10, time.Hour, nil, nil)
	if len(got) != 1 || got[0].SessionID != freshSID {
		t.Fatalf("want only the fresh session, got %+v", got)
	}
	if _, ok := dfsPathCache.Load(dfsCacheKey(root, staleEnc)); ok {
		t.Error("stale project dir was resolved; it should be skipped before resolution")
	}
	if _, ok := dfsPathCache.Load(dfsCacheKey(root, freshEnc)); !ok {
		t.Error("fresh project dir should have been resolved")
	}

	files := cachedJSONLFileInfo(filepath.Join(claudeDir, "projects", staleEnc))
	if len(files) != 1 {
		t.Fatalf("want 1 cached jsonl, got %d", len(files))
	}
	projDir := filepath.Join(claudeDir, "projects", staleEnc)
	if !hasJSONLSince(projDir, files[0].mtime) {
		t.Error("a jsonl exactly at the cutoff must count as recent")
	}
	if hasJSONLSince(projDir, files[0].mtime+1) {
		t.Error("a jsonl older than the cutoff must not count")
	}
}
