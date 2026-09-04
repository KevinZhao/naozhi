package memory

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	dashproject "github.com/naozhi/naozhi/internal/dashboard/project"
)

// negCacheTTL bounds how long a slug missing from every project directory is
// remembered as "not found", so repeated misses skip the full ReadDir scan.
const negCacheTTL = 30 * time.Second

// maxNegCacheEntries hard-caps the negative cache against slug-spray: at the
// cap we skip insertion rather than maintain an LRU.
const maxNegCacheEntries = 4096

// Handler serves GET /api/memory/{slug} for the dashboard "wiki link"
// preview: Claude's auto-memory uses [[slug]] cross-references and the
// inlineMd renderer turns them into hover cards backed by this endpoint.
//
// Lookup order (docs/rfc/memory-link-rendering.md): current project first,
// then every other project under projectsDir in lexicographic order.
//
// Path safety: slug must match memorySlugRE and the resolved path is
// re-checked against projectsDir; both gates must pass before reading.
type Handler struct {
	projectsDir    string
	currentProject string
	limiter        IPLimiter

	// Resolved projectsDir prefix cached at construction so the lexical
	// HasPrefix gate and the post-EvalSymlinks recheck share one immutable
	// base (#635). resolvedPrefixNoSep lets the root itself match exactly.
	resolvedPrefix      string
	resolvedPrefixNoSep string

	// Short-TTL negative cache keyed on slug: a full-scan miss is remembered
	// so repeated misses cannot DoS via ReadDir.
	negCacheMu sync.RWMutex
	negCache   map[string]time.Time
}

var memorySlugRE = regexp.MustCompile(`^[a-zA-Z0-9_\-]{1,64}$`)

// memoryProjectDirRE locks the shape of a `~/.claude/projects/<name>` entry
// (`-` + slash-replaced CWD). Defence in depth: entries with `..`, separators
// or control bytes never reach filepath.Join; non-matching entries are
// skipped since they cannot be Claude project dirs (#467).
var memoryProjectDirRE = regexp.MustCompile(`^-[a-zA-Z0-9_][a-zA-Z0-9._\-]{0,255}$`)

// utf8BOM is defined as bytes (not a string literal) so a literal BOM never
// appears in this Go source file — the compiler rejects mid-file BOM.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

const (
	MemoryLimiterRate  = 10
	MemoryLimiterBurst = 20

	// maxMemoryFileBytes caps a single memory .md read; larger files are
	// truncated and the response carries Truncated:true so the client can
	// show a "(已截断)" hint instead of silently losing the tail (#1044).
	maxMemoryFileBytes = 256 * 1024
)

var errMemoryPathEscape = errors.New("path escapes projects dir")

// New constructs a memory Handler. projectsDir and limiter are injected by
// the server package so this sub-package never reverse-imports it.
func New(projectsDir string, limiter IPLimiter) *Handler {
	dir := projectsDir
	// Canonicalise projectsDir so the prefix check in tryRead compares the
	// resolved leaf against a resolved root (symlinked ~/.claude etc.).
	// Best-effort: a not-yet-existing dir keeps the cleaned raw path.
	if dir != "" {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			dir = r
		}
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		dir = filepath.Clean(dir)
	}
	cur := encodeCurrentProjectDir(dir)
	prefixNoSep := strings.TrimRight(filepath.Clean(dir), string(filepath.Separator))
	prefix := prefixNoSep
	if prefix != "" {
		prefix += string(filepath.Separator)
	}
	return &Handler{
		projectsDir:         dir,
		currentProject:      cur,
		limiter:             limiter,
		resolvedPrefix:      prefix,
		resolvedPrefixNoSep: prefixNoSep,
	}
}

// encodeCurrentProjectDir maps the current working directory to the directory
// name Claude uses under ~/.claude/projects. Returns "" when the project
// memory dir is missing.
func encodeCurrentProjectDir(projectsDir string) string {
	if projectsDir == "" {
		return ""
	}
	pwd, err := os.Getwd()
	if err != nil || pwd == "" {
		return ""
	}
	encoded := "-" + strings.ReplaceAll(strings.TrimPrefix(pwd, "/"), "/", "-")
	candidate := filepath.Join(projectsDir, encoded, "memory")
	if st, err := os.Stat(candidate); err == nil && st.IsDir() {
		return encoded
	}
	return ""
}

type memoryResponse struct {
	Found       bool   `json:"found"`
	Slug        string `json:"slug"`
	Scope       string `json:"scope,omitempty"`
	Project     string `json:"project,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"`
	Body        string `json:"body,omitempty"`
	// Truncated signals the source exceeded maxMemoryFileBytes and Body holds
	// only the prefix (#1044).
	Truncated bool `json:"truncated,omitempty"`
}

func (h *Handler) HandleGet(w http.ResponseWriter, r *http.Request) {
	if !h.limiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
		return
	}
	slug := r.PathValue("slug")
	if !memorySlugRE.MatchString(slug) {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid_slug"})
		return
	}
	if h.projectsDir == "" {
		w.Header().Set("Cache-Control", "private, max-age=30")
		httputil.WriteJSON(w, memoryResponse{Found: false, Slug: slug})
		return
	}

	resp, err := h.lookup(slug)
	if err != nil {
		if errors.Is(err, errMemoryPathEscape) {
			httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid_slug"})
			return
		}
		httputil.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "io"})
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=30")
	httputil.WriteJSON(w, resp)
}

func (h *Handler) lookup(slug string) (memoryResponse, error) {
	if h.currentProject != "" {
		hit, err := h.tryRead(h.currentProject, slug)
		if err != nil {
			return memoryResponse{}, err
		}
		if hit != nil {
			hit.Scope = "current"
			return *hit, nil
		}
	}

	// Negative-cache hit within TTL: every project dir was already scanned.
	h.negCacheMu.RLock()
	if deadline, ok := h.negCache[slug]; ok && time.Now().Before(deadline) {
		h.negCacheMu.RUnlock()
		return memoryResponse{Found: false, Slug: slug}, nil
	}
	h.negCacheMu.RUnlock()

	entries, err := os.ReadDir(h.projectsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return memoryResponse{Found: false, Slug: slug}, nil
		}
		return memoryResponse{}, err
	}
	names := make([]string, 0, len(entries))
	for _, ent := range entries {
		if !ent.IsDir() || ent.Name() == h.currentProject {
			continue
		}
		// Skip names that cannot be a Claude-encoded project dir before they
		// reach tryRead's filepath.Join (#467).
		if !memoryProjectDirRE.MatchString(ent.Name()) {
			continue
		}
		names = append(names, ent.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		hit, err := h.tryRead(name, slug)
		if err != nil {
			return memoryResponse{}, err
		}
		if hit != nil {
			hit.Scope = "external"
			hit.Project = name
			return *hit, nil
		}
	}

	// Full scan missed: record a negative entry, sweeping expired ones first
	// so the map cannot grow without bound.
	h.negCacheMu.Lock()
	if h.negCache == nil {
		h.negCache = make(map[string]time.Time)
	} else {
		now := time.Now()
		for k, dl := range h.negCache {
			if now.After(dl) {
				delete(h.negCache, k)
			}
		}
	}
	// Insert only while below the hard cap (slug-spray defence).
	if len(h.negCache) < maxNegCacheEntries {
		h.negCache[slug] = time.Now().Add(negCacheTTL)
	}
	h.negCacheMu.Unlock()

	return memoryResponse{Found: false, Slug: slug}, nil
}

func (h *Handler) tryRead(projectDir, slug string) (*memoryResponse, error) {
	// Pin every Join input to the encoder alphabet (see
	// encodeCurrentProjectDir). Traversal/separator bytes are loud
	// (errMemoryPathEscape, matching the lexical gate below); any other
	// non-matching name is a benign miss (nil, nil) (#467).
	if strings.Contains(projectDir, "..") ||
		strings.ContainsAny(projectDir, `/\`) {
		return nil, errMemoryPathEscape
	}
	if !memoryProjectDirRE.MatchString(projectDir) {
		return nil, nil
	}
	full := filepath.Join(h.projectsDir, projectDir, "memory", slug+".md")
	clean := filepath.Clean(full)

	// Defence in depth: re-verify the path stays inside projectsDir using the
	// construction-time prefix shared with the post-EvalSymlinks recheck (#635).
	prefix := h.resolvedPrefix
	prefixNoSep := h.resolvedPrefixNoSep
	if !strings.HasPrefix(clean, prefix) {
		return nil, errMemoryPathEscape
	}

	// Resolve symlinks and re-check the prefix: a symlink at .../memory or at
	// the slug file could redirect the read outside projectsDir without
	// changing the lexical path. Missing files and escaping targets are both
	// reported as "not found" so no signal leaks about the symlink target.
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if !strings.HasPrefix(resolved, prefix) && resolved != prefixNoSep {
		return nil, nil
	}

	// Cap the read at maxMemoryFileBytes (reads cap+1 to distinguish
	// "exactly at cap" from truncation) (#1044).
	raw, truncated, err := readCappedMemoryFile(resolved, int64(maxMemoryFileBytes))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	meta, body := parseMemoryFrontmatter(raw)
	// Memory files can absorb attacker-influenced workspace content: scrub
	// control / bidi runes from every free-text field before the wire.
	resp := &memoryResponse{
		Found:       true,
		Slug:        slug,
		Name:        sanitizeWireText(meta.name),
		Description: sanitizeWireText(meta.description),
		Type:        sanitizeWireText(meta.typ),
		Body:        sanitizeWireText(body),
		Truncated:   truncated,
	}
	return resp, nil
}

// readCappedMemoryFile reads up to capBytes from path; truncated=true when
// the file was larger. Errors (including os.ErrNotExist) propagate unchanged.
func readCappedMemoryFile(path string, capBytes int64) ([]byte, bool, error) {
	// Open via OpenWorkspaceFile (O_NOFOLLOW) rather than os.Open to close the
	// TOCTOU window between the caller's EvalSymlinks check and the open; then
	// Lstat-before / Fstat-after SameFile pins the descriptor to the exact
	// inode validated above (a swap to a different regular file is not caught
	// by O_NOFOLLOW alone). os.ErrNotExist still propagates unchanged.
	li, err := os.Lstat(path)
	if err != nil {
		return nil, false, err
	}
	f, err := dashproject.OpenWorkspaceFile(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !os.SameFile(li, fi) {
		return nil, false, os.ErrNotExist
	}
	// Read capBytes+1 to detect overflow without a racy extra Stat.
	lr := &io.LimitedReader{R: f, N: capBytes + 1}
	raw, err := io.ReadAll(lr)
	if err != nil {
		return nil, false, err
	}
	if int64(len(raw)) > capBytes {
		return raw[:capBytes], true, nil
	}
	return raw, false, nil
}

type memoryFrontmatter struct {
	name        string
	description string
	typ         string
}

// parseMemoryFrontmatter strips a leading YAML frontmatter block if present
// and returns the body. Hand-rolled to avoid a yaml.v3 dependency for what
// is otherwise a 5-line schema.
func parseMemoryFrontmatter(raw []byte) (memoryFrontmatter, string) {
	var meta memoryFrontmatter
	raw = bytes.TrimPrefix(raw, utf8BOM)
	s := string(raw)
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return meta, strings.TrimLeft(s, "\r\n")
	}
	rest := s
	if strings.HasPrefix(rest, "---\r\n") {
		rest = rest[5:]
	} else {
		rest = rest[4:]
	}
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return meta, strings.TrimLeft(s, "\r\n")
	}
	front := rest[:idx]
	body := rest[idx+len("\n---"):]
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[i+1:]
	} else {
		body = ""
	}

	inMetadata := false
	for _, line := range strings.Split(front, "\n") {
		raw := strings.TrimRight(line, "\r")
		if strings.HasPrefix(raw, "metadata:") {
			inMetadata = true
			continue
		}
		if inMetadata && (strings.HasPrefix(raw, "  ") || strings.HasPrefix(raw, "\t")) {
			k, v, ok := splitYAMLKV(strings.TrimSpace(raw))
			if !ok {
				continue
			}
			if k == "type" {
				meta.typ = v
			}
			continue
		}
		inMetadata = false
		k, v, ok := splitYAMLKV(raw)
		if !ok {
			continue
		}
		switch k {
		case "name":
			meta.name = v
		case "description":
			meta.description = v
		case "type":
			// Top-level `type:` (the form Claude Code writes) is honoured, but
			// never overrides a nested `metadata.type` (#2433).
			if meta.typ == "" {
				meta.typ = v
			}
		}
	}
	return meta, strings.TrimLeft(body, "\r\n")
}

func splitYAMLKV(line string) (string, string, bool) {
	i := strings.IndexByte(line, ':')
	if i <= 0 {
		return "", "", false
	}
	k := strings.TrimSpace(line[:i])
	v := strings.TrimSpace(line[i+1:])
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			v = v[1 : len(v)-1]
		}
	}
	return k, v, true
}
