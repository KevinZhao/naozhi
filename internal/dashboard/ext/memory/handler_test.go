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
)

// memoryTestHandler builds a handler with a temp projects dir + permissive
// limiter so tests don't false-positive on rate limits.
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
		limiter:             alwaysAllowLimiter{},
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
	// create the projects dir but no memories
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
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

func TestMemoryHandler_PathEscapeBlockedAtRegex(t *testing.T) {
	dir := t.TempDir()
	// drop a sentinel file outside the projects dir
	outside := filepath.Join(filepath.Dir(dir), "secret.md")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)
	h := memoryTestHandler(t, dir, "")

	// Slugs containing path traversal are rejected by the regex and never
	// reach the filesystem read. We assert 400 + invalid_slug.
	req := httptest.NewRequest(http.MethodGet, "/api/memory/x", nil)
	req.SetPathValue("slug", "../secret")
	w := httptest.NewRecorder()
	h.HandleGet(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid_slug") {
		t.Errorf("body = %s, want invalid_slug error", w.Body.String())
	}
}

// TestMemoryHandler_PathEscapeBlockedAtTryRead verifies the second-line
// defence: if the slug regex were ever loosened to admit "." or "/", the
// HasPrefix gate inside tryRead must still reject the read.
func TestMemoryHandler_PathEscapeBlockedAtTryRead(t *testing.T) {
	dir := t.TempDir()
	h := memoryTestHandler(t, dir, "")

	// Manually call tryRead with a project name that contains "..". The
	// regex normally guards the slug, but tryRead is what the second-line
	// defence covers, so we feed a clean slug + crafted projectDir.
	_, err := h.tryRead("../..", "anything")
	if err == nil {
		t.Fatalf("expected errMemoryPathEscape")
	}
	if !errorsIs(err, errMemoryPathEscape) {
		t.Errorf("err = %v, want errMemoryPathEscape", err)
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

func TestParseMemoryFrontmatter_BOMStripped(t *testing.T) {
	bom := []byte{0xEF, 0xBB, 0xBF}
	input := append(bom, []byte("---\nname: bom\n---\nbody\n")...)
	meta, body := parseMemoryFrontmatter(input)
	if meta.name != "bom" {
		t.Errorf("name = %q, want bom", meta.name)
	}
	if body != "body\n" {
		t.Errorf("body = %q", body)
	}
}

func TestEncodeCurrentProjectDir_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	pwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	encoded := "-" + filepath.Clean(pwd)
	encoded = "-" + filepath.ToSlash(pwd)[1:] // mirror the encoder
	// strip slashes
	for i := 0; i < len(encoded); i++ {
		if encoded[i] == '/' {
			encoded = encoded[:i] + "-" + encoded[i+1:]
		}
	}

	// create the memory dir for that encoded name
	if err := os.MkdirAll(filepath.Join(dir, encoded, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := encodeCurrentProjectDir(dir)
	if got != encoded {
		t.Errorf("encodeCurrentProjectDir(%q) = %q, want %q", dir, got, encoded)
	}
}

// errorsIs is a tiny shim used to keep the test file's import surface
// minimal (errors.Is would also work; both are fine).
func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestMemoryHandler_OversizeFileTruncated covers R240-SEC-11 / #1044: a
// memory file larger than maxMemoryFileBytes must be capped at the limit
// and the response must carry truncated=true so the client can hint the
// user instead of silently losing the tail.
func TestMemoryHandler_OversizeFileTruncated(t *testing.T) {
	dir := t.TempDir()
	// Build content well beyond the cap so we positively cross the limit
	// even after frontmatter parsing strips a header (none here — plain
	// body only). 2× cap is enough to detect any off-by-one.
	big := strings.Repeat("a", maxMemoryFileBytes*2)
	writeMemoryFile(t, dir, "-cur", "huge_memory", big)
	h := memoryTestHandler(t, dir, "-cur")

	w, resp, _ := callMemoryHandler(t, h, "huge_memory")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	if !resp.Found {
		t.Fatalf("resp.Found = false, want true")
	}
	if !resp.Truncated {
		t.Errorf("resp.Truncated = false, want true for >cap file")
	}
	// Body holds the cap-capped prefix (no frontmatter on this fixture, so
	// body == raw[:cap]).
	if got, want := len(resp.Body), maxMemoryFileBytes; got != want {
		t.Errorf("len(Body) = %d, want %d (cap)", got, want)
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
	// Create the projects dir but put no memory file for slug "ghost_slug".
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
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

// TestMemoryHandler_AtCapNotTruncated guards the boundary: a file exactly
// at maxMemoryFileBytes must NOT be flagged truncated.
func TestMemoryHandler_AtCapNotTruncated(t *testing.T) {
	dir := t.TempDir()
	exact := strings.Repeat("b", maxMemoryFileBytes)
	writeMemoryFile(t, dir, "-cur", "boundary_mem", exact)
	h := memoryTestHandler(t, dir, "-cur")

	_, resp, _ := callMemoryHandler(t, h, "boundary_mem")
	if !resp.Found {
		t.Fatalf("resp.Found = false")
	}
	if resp.Truncated {
		t.Errorf("file exactly at cap must NOT be truncated, got Truncated=true")
	}
	if got := len(resp.Body); got != maxMemoryFileBytes {
		t.Errorf("len(Body) = %d, want %d", got, maxMemoryFileBytes)
	}
}

// TestNegCache_SweepOnWrite verifies R164029-CR-2: expired entries are
// evicted from the negative cache on the next write, preventing unbounded
// map growth when an attacker sprays unique slugs.
func TestNegCache_SweepOnWrite(t *testing.T) {
	dir := t.TempDir()
	// No memory files — every slug lookup will miss and populate negCache.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
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
