package memory

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
)

// memoryTestHandler builds a handler with a temp projects dir.
//
// projectsDir is canonicalised the same way New does it — without
// this, on macOS t.TempDir() returns /var/folders/... which is a symlink to
// /private/var/folders/...; tryRead's EvalSymlinks resolves the leaf to the
// /private/var/... form, and the prefix check (which compares against the
// unresolved root) rejects every legitimate read.
func memoryTestHandler(t *testing.T, projectsDir, currentProject string) *Handler {
	t.Helper()
	if projectsDir != "" {
		if r, err := filepath.EvalSymlinks(projectsDir); err == nil {
			projectsDir = r
		}
		if abs, err := filepath.Abs(projectsDir); err == nil {
			projectsDir = abs
		}
		projectsDir = filepath.Clean(projectsDir)
	}
	prefixNoSep := strings.TrimRight(filepath.Clean(projectsDir), string(filepath.Separator))
	prefix := prefixNoSep
	if prefix != "" {
		prefix += string(filepath.Separator)
	}
	return &Handler{
		projectsDir:         projectsDir,
		currentProject:      currentProject,
		resolvedPrefix:      prefix,
		resolvedPrefixNoSep: prefixNoSep,
	}
}

// writeMemoryFile drops a memory file under <projectsDir>/<project>/memory/<slug>.md.
func writeMemoryFile(t *testing.T, projectsDir, project, slug, content string) {
	t.Helper()
	dir := filepath.Join(projectsDir, project, "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, slug+".md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// callMemoryHandler runs the handler against an httptest recorder using
// http.ServeMux so PathValue("slug") populates correctly.
func callMemoryHandler(t *testing.T, h *Handler, slug string) (*httptest.ResponseRecorder, memoryResponse, map[string]string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/memory/{slug}", h.HandleGet)
	req := httptest.NewRequest(http.MethodGet, "/api/memory/"+slug, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		var resp memoryResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode resp: %v", err)
		}
		return w, resp, nil
	}
	var errBody map[string]string
	_ = json.NewDecoder(w.Body).Decode(&errBody)
	return w, memoryResponse{}, errBody
}

func TestMemoryHandler_HitCurrentProject(t *testing.T) {
	dir := t.TempDir()
	writeMemoryFile(t, dir, "-cur", "feedback_foo", `---
name: feedback-foo
description: example feedback
metadata:
  type: feedback
---

Body **markdown** here.
`)
	h := memoryTestHandler(t, dir, "-cur")

	w, resp, _ := callMemoryHandler(t, h, "feedback_foo")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !resp.Found || resp.Scope != "current" {
		t.Errorf("resp = %+v, want found=true scope=current", resp)
	}
	if resp.Description != "example feedback" {
		t.Errorf("description = %q", resp.Description)
	}
	if resp.Type != "feedback" {
		t.Errorf("type = %q", resp.Type)
	}
	if resp.Body != "Body **markdown** here.\n" {
		t.Errorf("body = %q", resp.Body)
	}
	if resp.Project != "" {
		t.Errorf("project should be empty for current scope, got %q", resp.Project)
	}
}

func TestMemoryHandler_FallsBackToExternalProject(t *testing.T) {
	dir := t.TempDir()
	// only the external project has the slug
	writeMemoryFile(t, dir, "-other-proj", "shared_slug", `---
name: shared-slug
description: lives in other proj
---
content
`)
	// current project exists but has no matching slug
	writeMemoryFile(t, dir, "-cur", "unrelated", "x")
	h := memoryTestHandler(t, dir, "-cur")

	w, resp, _ := callMemoryHandler(t, h, "shared_slug")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	if !resp.Found || resp.Scope != "external" || resp.Project != "-other-proj" {
		t.Errorf("resp = %+v, want external/-other-proj", resp)
	}
}

func TestMemoryHandler_NotFound(t *testing.T) {
	dir := t.TempDir()
	h := memoryTestHandler(t, dir, "")

	w, resp, _ := callMemoryHandler(t, h, "nope")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with found=false", w.Code)
	}
	if resp.Found {
		t.Errorf("resp.Found = true, want false")
	}
	if resp.Slug != "nope" {
		t.Errorf("slug echo = %q", resp.Slug)
	}
}

func TestMemoryHandler_RejectsInvalidSlug(t *testing.T) {
	dir := t.TempDir()
	h := memoryTestHandler(t, dir, "")

	cases := []string{
		"with.dot",               // dot
		"with/slash",             // slash
		"with..slash",            // dot dot
		"../secret",              // traversal never reaches the filesystem
		"a b",                    // space — although mux %20-decodes this anyway
		string(make([]byte, 65)), // length over limit
	}
	for _, slug := range cases {
		t.Run(slug, func(t *testing.T) {
			// mux strips empty path values so we cannot hit HandleGet
			// directly via mux for these — invoke handler with a manual
			// request that already has the PathValue set.
			req := httptest.NewRequest(http.MethodGet, "/api/memory/x", nil)
			req.SetPathValue("slug", slug)
			w := httptest.NewRecorder()
			h.HandleGet(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("slug %q: status = %d, want 400 (body=%s)",
					slug, w.Code, w.Body.String())
			}
		})
	}
}

func TestParseMemoryFrontmatter_Variants(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantName string
		wantDesc string
		wantType string
		wantBody string
	}{
		{
			name: "full frontmatter",
			input: `---
name: foo
description: bar baz
metadata:
  type: feedback
---

body line 1
body line 2
`,
			wantName: "foo",
			wantDesc: "bar baz",
			wantType: "feedback",
			wantBody: "body line 1\nbody line 2\n",
		},
		{
			name: "quoted values",
			input: `---
name: "foo"
description: 'with: colon'
---
body
`,
			wantName: "foo",
			wantDesc: "with: colon",
			wantBody: "body\n",
		},
		{
			name: "no frontmatter",
			input: `# Heading

content
`,
			wantBody: "# Heading\n\ncontent\n",
		},
		{
			name: "frontmatter only",
			input: `---
name: only
---
`,
			wantName: "only",
			wantBody: "",
		},
		{
			name: "malformed frontmatter (no closing fence)",
			input: `---
name: hanging
body never closed
`,
			wantBody: "---\nname: hanging\nbody never closed\n",
		},
		{
			name:     "BOM stripped",
			input:    "\xEF\xBB\xBF---\nname: bom\n---\nbody\n",
			wantName: "bom",
			wantBody: "body\n",
		},
		// #2433 item 6: memory files written by Claude Code use a top-level
		// `type:` key; the nested metadata form wins when both are present.
		{
			name:     "top-level type",
			input:    "---\nname: n\ndescription: d\ntype: feedback\noriginSessionId: abc\n---\nbody\n",
			wantName: "n",
			wantDesc: "d",
			wantType: "feedback",
			wantBody: "body\n",
		},
		{
			name:     "metadata type wins when after top-level",
			input:    "---\ntype: feedback\nmetadata:\n  type: project\n---\nbody\n",
			wantType: "project",
			wantBody: "body\n",
		},
		{
			name:     "metadata type wins when before top-level",
			input:    "---\nmetadata:\n  type: project\ntype: feedback\n---\nbody\n",
			wantType: "project",
			wantBody: "body\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta, body := parseMemoryFrontmatter([]byte(tc.input))
			if meta.name != tc.wantName {
				t.Errorf("name = %q, want %q", meta.name, tc.wantName)
			}
			if meta.description != tc.wantDesc {
				t.Errorf("desc = %q, want %q", meta.description, tc.wantDesc)
			}
			if meta.typ != tc.wantType {
				t.Errorf("type = %q, want %q", meta.typ, tc.wantType)
			}
			if body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
		})
	}
}

// TestMemoryHandler_TruncatedAtCap covers R240-SEC-11 / #1044: a memory
// file larger than maxMemoryFileBytes is capped at the limit and flagged
// truncated so the client can hint the user instead of silently losing the
// tail; one exactly at the cap is not flagged.
func TestMemoryHandler_TruncatedAtCap(t *testing.T) {
	for _, tc := range []struct {
		size      int
		truncated bool
	}{{maxMemoryFileBytes * 2, true}, {maxMemoryFileBytes, false}} {
		dir := t.TempDir()
		writeMemoryFile(t, dir, "-cur", "big_memory", strings.Repeat("a", tc.size))
		h := memoryTestHandler(t, dir, "-cur")

		w, resp, _ := callMemoryHandler(t, h, "big_memory")
		if w.Code != http.StatusOK || !resp.Found {
			t.Fatalf("size %d: status = %d found = %v; body=%s", tc.size, w.Code, resp.Found, w.Body.String())
		}
		// No frontmatter on this fixture, so body == raw[:cap].
		if resp.Truncated != tc.truncated || len(resp.Body) != maxMemoryFileBytes {
			t.Errorf("size %d: truncated = %v, len(Body) = %d; want %v, %d", tc.size, resp.Truncated, len(resp.Body), tc.truncated, maxMemoryFileBytes)
		}
	}
}

// TestMemoryHandler_NegativeCacheSkipsReadDir verifies R20260602141221-SEC-10:
// after a full-scan miss, the negative cache entry causes subsequent requests
// within the TTL to return "not found" without calling os.ReadDir again.
//
// We prove the skip by removing the projects dir after the first miss; if the
// second call hit the disk it would return an I/O error (500), but the negative
// cache must intercept it and return 200/found=false instead.
func TestMemoryHandler_NegativeCacheSkipsReadDir(t *testing.T) {
	dir := t.TempDir()
	h := memoryTestHandler(t, dir, "")

	// First call: full ReadDir scan, nothing found — populates negative cache.
	w1, resp1, _ := callMemoryHandler(t, h, "ghost_slug")
	if w1.Code != http.StatusOK {
		t.Fatalf("first call: status = %d, want 200; body=%s", w1.Code, w1.Body.String())
	}
	if resp1.Found {
		t.Fatalf("first call: resp.Found = true, want false")
	}

	// Remove the projects dir entirely so any ReadDir attempt would error.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	// Second call within TTL: must hit negative cache and return 200/found=false
	// without touching the (now-deleted) dir.
	w2, resp2, _ := callMemoryHandler(t, h, "ghost_slug")
	if w2.Code != http.StatusOK {
		t.Fatalf("second call: status = %d (want 200/not-found via neg cache); body=%s",
			w2.Code, w2.Body.String())
	}
	if resp2.Found {
		t.Errorf("second call: resp.Found = true, want false")
	}
}

// TestNegCache_SweepOnWrite verifies R164029-CR-2: expired entries are
// evicted from the negative cache on the next write, preventing unbounded
// map growth when an attacker sprays unique slugs.
func TestNegCache_SweepOnWrite(t *testing.T) {
	dir := t.TempDir()
	h := memoryTestHandler(t, dir, "")

	// Pre-populate negCache with three already-expired entries by writing
	// them directly (using a deadline in the past).
	past := time.Now().Add(-time.Second)
	h.negCacheMu.Lock()
	h.negCache = map[string]time.Time{
		"stale_slug_a": past,
		"stale_slug_b": past,
		"stale_slug_c": past,
	}
	h.negCacheMu.Unlock()

	// A single miss on a new slug must trigger sweep-on-write, removing the
	// three stale entries and then adding the new one — net size == 1.
	callMemoryHandler(t, h, "new_slug_x")

	h.negCacheMu.Lock()
	size := len(h.negCache)
	_, hasStale := h.negCache["stale_slug_a"]
	_, hasNew := h.negCache["new_slug_x"]
	h.negCacheMu.Unlock()

	if size != 1 {
		t.Errorf("negCache len = %d after sweep, want 1 (stale entries not evicted)", size)
	}
	if hasStale {
		t.Errorf("stale entry still present after sweep")
	}
	if !hasNew {
		t.Errorf("new entry missing after write")
	}
}

// TestNegCache_MaxEntries verifies R220123-SEC-4: the negative cache does not
// grow beyond maxNegCacheEntries even when many unique slugs miss in rapid
// succession (defence-in-depth against slug-spray attacks).
func TestNegCache_MaxEntries(t *testing.T) {
	dir := t.TempDir()
	h := memoryTestHandler(t, dir, "")

	// Pre-fill the cache to exactly the cap with live (non-expired) entries.
	h.negCacheMu.Lock()
	h.negCache = make(map[string]time.Time, maxNegCacheEntries)
	for i := 0; i < maxNegCacheEntries; i++ {
		h.negCache[fmt.Sprintf("slug_%d", i)] = time.Now().Add(time.Minute)
	}
	h.negCacheMu.Unlock()

	// A miss on a brand-new slug should NOT insert — the cap must be respected.
	callMemoryHandler(t, h, "overflow_slug")

	h.negCacheMu.Lock()
	size := len(h.negCache)
	_, hasOverflow := h.negCache["overflow_slug"]
	h.negCacheMu.Unlock()

	if size != maxNegCacheEntries {
		t.Errorf("negCache len = %d after cap-overflow attempt, want %d", size, maxNegCacheEntries)
	}
	if hasOverflow {
		t.Errorf("overflow_slug was inserted despite cap being reached")
	}
}

// TestNegCache_ConcurrentReads verifies R20260602190132-GO-3: the negative
// cache uses sync.RWMutex so multiple goroutines may read concurrently without
// data races. Run with -race to detect any violation.
func TestNegCache_ConcurrentReads(t *testing.T) {
	dir := t.TempDir()
	h := memoryTestHandler(t, dir, "")

	// Seed the negative cache with a valid entry so the read path (RLock) fires.
	h.negCacheMu.Lock()
	h.negCache = map[string]time.Time{
		"seeded_slug": time.Now().Add(time.Minute),
	}
	h.negCacheMu.Unlock()

	// Launch N goroutines that all read the negative cache simultaneously.
	// R220123-CR-1: goroutines must not call t.Fatal/t.Errorf — only the test
	// goroutine may call testing.T methods. Collect (code, found) pairs into a
	// buffered channel and assert in the main goroutine after all complete.
	type result struct {
		code  int
		found bool
	}
	const N = 20
	results := make(chan result, N)
	for i := 0; i < N; i++ {
		go func() {
			// Build and serve the request directly — no t.* calls inside the goroutine.
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/memory/{slug}", h.HandleGet)
			req := httptest.NewRequest(http.MethodGet, "/api/memory/seeded_slug", nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			var resp memoryResponse
			_ = json.NewDecoder(w.Body).Decode(&resp)
			results <- result{code: w.Code, found: resp.Found}
		}()
	}
	for i := 0; i < N; i++ {
		r := <-results
		if r.code != http.StatusOK {
			t.Errorf("concurrent read %d: status = %d, want 200", i, r.code)
		}
		if r.found {
			t.Errorf("concurrent read %d: resp.Found = true, want false", i)
		}
	}
}

// TestReadCappedMemoryFile_NormalPath exercises the happy path for
// readCappedMemoryFile to ensure the R20260606-SEC-6 SameFile check does not
// block legitimate reads of regular files.
func TestReadCappedMemoryFile_NormalPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := "hello from memory file\n"
	path := filepath.Join(dir, "mem.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, truncated, _, err := readCappedMemoryFile(path, int64(maxMemoryFileBytes))
	if err != nil {
		t.Fatalf("readCappedMemoryFile: %v", err)
	}
	if truncated {
		t.Errorf("truncated = true, want false for small file")
	}
	if string(got) != content {
		t.Errorf("content = %q, want %q", got, content)
	}
}

// TestReadCappedMemoryFile_SymlinkRejectedByONofollow (R20260606-SEC-6): a
// symlink swapped in between EvalSymlinks and the open is refused by
// O_NOFOLLOW, so the caller reads "not found" rather than the wrong file.
func TestReadCappedMemoryFile_SymlinkRejectedByONofollow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// target file that the symlink will point to
	target := filepath.Join(dir, "target.md")
	if err := os.WriteFile(target, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	// link inside the projects dir pointing at target
	link := filepath.Join(dir, "link.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlink not supported:", err)
	}

	// readCappedMemoryFile must not return the target's bytes when fed the
	// symlink path directly; O_NOFOLLOW blocks the open at the kernel boundary.
	_, _, _, err := readCappedMemoryFile(link, int64(maxMemoryFileBytes))
	if err == nil {
		t.Error("expected an error when opening a symlink path, got nil")
	}
}

// The current project's directory is found under the CLI's own slug, which
// replaces every non-alphanumeric byte — not only '/' — so a cwd with '.' or
// '_' in it still finds its memory.
func TestEncodeCurrentProjectDir_UsesTheCLISlug(t *testing.T) {
	work := filepath.Join(t.TempDir(), "my.proj_x")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	pwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	slug := claudefs.ProjectSlug(pwd)
	if strings.ContainsAny(slug, "._") {
		t.Fatalf("fixture: slug %q should have '.' and '_' replaced", slug)
	}
	projects := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projects, slug, "memory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := encodeCurrentProjectDir(projects); got != slug {
		t.Errorf("encodeCurrentProjectDir = %q, want the CLI slug %q", got, slug)
	}
}
