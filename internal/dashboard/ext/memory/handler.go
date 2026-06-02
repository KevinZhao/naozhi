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

// Handler serves GET /api/memory/{slug} for the dashboard "wiki link"
// preview. Claude's auto-memory uses [[slug]] cross-references in stored
// memory files (and occasionally leaks them into chat output); the dashboard
// inlineMd renderer turns them into hover cards backed by this endpoint.
//
// Lookup order — see docs/rfc/memory-link-rendering.md:
//  1. current project (best-effort PWD encoding)
//  2. all other projects under projectsDir, in lexicographic directory order
//
// Path safety: the slug is validated against memorySlugRE (alphanumeric / _ / -,
// 1-64 chars) and the resolved path is re-checked with strings.HasPrefix on
// projectsDir; both gates must pass before we read.
// negCacheTTL is the duration for which a slug that was not found in any
// project directory is remembered as "not found", avoiding repeated full
// ReadDir scans within the same TTL window.
// R20260602141221-SEC-10.
const negCacheTTL = 30 * time.Second

type Handler struct {
	projectsDir    string
	currentProject string
	limiter        IPLimiter

	// R242-SEC-7 (#635): cache the resolved-prefix at construction time so the
	// runtime base used for both the lexical HasPrefix gate and the post-
	// EvalSymlinks recheck is identical and immutable. Recomputing the prefix
	// per-request from h.projectsDir is functionally equivalent (h.projectsDir
	// is set once at construction and never mutated), but caching here pins
	// the base as a property of the handler — future code that repoints
	// projectsDir cannot drift the gates apart, and there's no chance the
	// prefix is rebuilt under a partially-set field.
	//
	// resolvedPrefix already carries the trailing separator; resolvedPrefixNoSep
	// is the same value with the trailing separator stripped, so a direct
	// equality match with the projects root itself (resolved == prefixNoSep)
	// stays accepted exactly as before.
	resolvedPrefix      string
	resolvedPrefixNoSep string

	// R20260602141221-SEC-10: short-TTL negative cache keyed on slug. When a
	// full ReadDir scan finds no match, we record the deadline here so
	// subsequent requests within the TTL skip the expensive disk scan and
	// return "not found" immediately, preventing DoS via repeated cache-miss.
	negCacheMu sync.Mutex
	negCache   map[string]time.Time
}

var memorySlugRE = regexp.MustCompile(`^[a-zA-Z0-9_\-]{1,64}$`)

// memoryProjectDirRE locks the shape of a Claude `~/.claude/projects/<name>`
// directory entry. Claude encodes the project path as `-` + slash-replaced
// CWD, so legitimate names look like `-home-user-workspace-foo` — leading
// dash, alnum + `_-.` thereafter, capped at a generous length.
//
// R241-SEC-6 (#467): defence in depth. tryRead joins the entry name into
// the lookup path; even though we only iterate `os.ReadDir(projectsDir)`
// (so an attacker would need write access to ~/.claude/projects to plant
// a malicious entry), an entry whose name carried `..` separators or
// embedded NUL / control bytes could still influence the resolved path
// or pollute audit logs. Filtering at iteration time keeps every Join
// input within the alphabet the encoder produces. Non-matching entries
// are silently skipped — they cannot be Claude project dirs and any
// memory file inside them would not be discoverable through the regular
// lookup path either.
var memoryProjectDirRE = regexp.MustCompile(`^-[a-zA-Z0-9_][a-zA-Z0-9._\-]{0,255}$`)

// utf8BOM is defined as bytes (not a string literal) so a literal BOM never
// appears in this Go source file — the compiler rejects mid-file BOM.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

const (
	MemoryLimiterRate  = 10
	MemoryLimiterBurst = 20

	// maxMemoryFileBytes caps the size of a single memory .md read. Auto-memory
	// slugs are normally small (front-matter + a few KB of narrative); a
	// pathological case (multi-MB hand-written notes, or a stray binary
	// dropped under projects/<proj>/memory/) would otherwise be slurped into
	// RAM, JSON-marshalled, and re-shipped on every hover preview, amplifying
	// cost N times per dashboard tab. 256 KB covers any realistic memory file
	// and keeps peak alloc bounded. Files larger than the cap are truncated
	// at the cap and the response carries Truncated:true so the client can
	// show "(展开为大文件,内容已截断)" instead of silently losing the tail.
	// R240-SEC-11 / #1044.
	maxMemoryFileBytes = 256 * 1024
)

var errMemoryPathEscape = errors.New("path escapes projects dir")

// New constructs a memory Handler.
//
// Phase 3c (server-split-phase4-design.md §6.5 Plan B): projectsDir +
// limiter are now injected from the server package so this sub-package
// doesn't reverse-import internal/server's resolveClaudeProjectsDir or
// newIPLimiterWithProxy. server.New computes the resolved+symlink-followed
// path once at boot and passes it here.
func New(projectsDir string, limiter IPLimiter) *Handler {
	dir := projectsDir
	// R240-SEC-1: canonicalise projectsDir at construction. If the dir is itself
	// reachable via a symlinked component (Docker bind-mount, AMI-customised
	// layout, ~/.claude → /var/data/.claude on macOS), the prefix check inside
	// tryRead would compare a resolved file path against an unresolved root and
	// reject every legitimate read. EvalSymlinks here aligns the root with the
	// resolved leaf path used by the symmetric check below. Best-effort: if the
	// dir does not exist yet (fresh install) we keep the cleaned raw path; the
	// check still applies once the dir materialises.
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
	// Truncated signals that the source file exceeded maxMemoryFileBytes
	// and Body holds only the prefix. Client may surface a "(已截断)" hint.
	// R240-SEC-11 / #1044.
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

	// R20260602141221-SEC-10: check negative cache before doing a full ReadDir.
	// A miss within the TTL means we already scanned all project dirs and found
	// nothing; skip the expensive scan and return "not found" immediately.
	h.negCacheMu.Lock()
	if deadline, ok := h.negCache[slug]; ok && time.Now().Before(deadline) {
		h.negCacheMu.Unlock()
		return memoryResponse{Found: false, Slug: slug}, nil
	}
	h.negCacheMu.Unlock()

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
		// R241-SEC-6 (#467): skip entries whose names cannot be a
		// Claude-encoded project dir. Disk-controlled names containing
		// `..`, slashes, or control bytes would otherwise reach
		// tryRead's filepath.Join and depend solely on the lexical
		// HasPrefix check below to stay rooted. The regex is the first
		// line of defence; the prefix + EvalSymlinks checks remain.
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

	// Nothing found in full scan — record negative cache entry.
	h.negCacheMu.Lock()
	if h.negCache == nil {
		h.negCache = make(map[string]time.Time)
	}
	h.negCache[slug] = time.Now().Add(negCacheTTL)
	h.negCacheMu.Unlock()

	return memoryResponse{Found: false, Slug: slug}, nil
}

func (h *Handler) tryRead(projectDir, slug string) (*memoryResponse, error) {
	// R241-SEC-6 (#467): pin every Join input to the alphabet Claude's
	// project-dir encoder produces (see encodeCurrentProjectDir). The
	// only call sites pass either h.currentProject or an entry returned
	// by os.ReadDir, but a future caller plumbing user input through
	// projectDir would otherwise reach filepath.Join with no shape
	// gate.
	//
	// Two failure modes, two response shapes — preserves the existing
	// errMemoryPathEscape contract that callers already test:
	//
	//   • Name contains traversal/separator bytes ("..", "/", "\\") —
	//     return errMemoryPathEscape so a deliberate attack-shaped
	//     input is loud (matches the lexical-prefix check below).
	//   • Name simply doesn't match the encoder alphabet (e.g. random
	//     stray dirs disk-write planted, ent.Name() that didn't pass
	//     the iteration filter for some other reason) — return
	//     (nil, nil) the same way "slug not found" does, so a benign
	//     non-Claude entry never surfaces an error to the dashboard.
	if strings.Contains(projectDir, "..") ||
		strings.ContainsAny(projectDir, `/\`) {
		return nil, errMemoryPathEscape
	}
	if !memoryProjectDirRE.MatchString(projectDir) {
		return nil, nil
	}
	full := filepath.Join(h.projectsDir, projectDir, "memory", slug+".md")
	clean := filepath.Clean(full)

	// Defence in depth: even though slug is regex-locked, re-verify the
	// resolved path stays inside projectsDir.
	//
	// R242-SEC-7 (#635): use the construction-time cached prefix so the
	// lexical gate and the post-EvalSymlinks recheck below share an
	// identical, immutable base. Rebuilding the prefix per-request was
	// functionally equivalent here (h.projectsDir is set once and never
	// mutated) but kept the door open for a future caller mutating it,
	// or for the two derivations to drift via filepath.Clean differences.
	prefix := h.resolvedPrefix
	prefixNoSep := h.resolvedPrefixNoSep
	if !strings.HasPrefix(clean, prefix) {
		return nil, errMemoryPathEscape
	}

	// R240-SEC-2: resolve symlinks before reading. The clean-prefix check above
	// catches lexical traversal, but a symlink at <projectsDir>/<proj>/memory or
	// at the slug file itself could redirect the read to /etc/shadow without
	// changing the lexical path. EvalSymlinks plus a re-check of the resolved
	// path's prefix closes that gap. Non-existent files return (nil, nil) just
	// like the ReadFile path below, so callers see them as "slug not found"
	// rather than an error. A resolved path that escapes the projects dir is
	// treated as a miss too — defence in depth, and avoids leaking any signal
	// about whether the symlink target exists.
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

	// R240-SEC-11 / #1044: cap memory file reads at maxMemoryFileBytes so a
	// pathological multi-MB file cannot be slurped+JSON-marshalled+re-shipped
	// on every hover. Use os.Open + io.ReadAll on a LimitedReader (cap+1) so
	// we can distinguish "exactly at cap" (legitimate boundary) from "exceeded
	// cap" (truncation). Non-existent files match the prior ReadFile path.
	raw, truncated, err := readCappedMemoryFile(resolved, int64(maxMemoryFileBytes))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	meta, body := parseMemoryFrontmatter(raw)
	// R103901-SEC-4: memory files are Claude-CLI-authored and can absorb
	// attacker-influenced workspace content, so scrub control / bidi runes
	// out of every free-text field before it reaches the dashboard wire.
	resp := &memoryResponse{
		Found:       true,
		Slug:        slug,
		Name:        sanitizeWireText(meta.name),
		Description: sanitizeWireText(meta.description),
		Type:        meta.typ,
		Body:        sanitizeWireText(body),
		Truncated:   truncated,
	}
	return resp, nil
}

// readCappedMemoryFile reads up to capBytes bytes from path. If the
// underlying file is larger than capBytes, returns the first capBytes bytes
// plus truncated=true; otherwise returns the full content with
// truncated=false. Errors (including os.ErrNotExist) propagate unchanged so
// the caller's existing branches keep working.
func readCappedMemoryFile(path string, capBytes int64) ([]byte, bool, error) {
	// R20260531-SEC-5: open with O_NOFOLLOW (via OpenWorkspaceFile) instead
	// of os.Open. The caller resolves `path` through filepath.EvalSymlinks and
	// re-checks the projects-dir prefix, but a plain os.Open here leaves a
	// TOCTOU window in which an attacker swaps the final component for a
	// symlink between that check and this open. OpenWorkspaceFile refuses a
	// final-component symlink kernel-atomically. os.ErrNotExist still
	// propagates unchanged (callers collapse it to "slug not found"); an
	// ELOOP symlink-swap surfaces as a generic error and fails closed.
	f, err := dashproject.OpenWorkspaceFile(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	// Read capBytes+1 to detect overflow without a separate Stat — Stat may
	// race with the read on a live FS (file growing/shrinking) and an extra
	// syscall is wasteful for the common small-file path.
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
