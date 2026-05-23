package server

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// MemoryHandler serves GET /api/memory/{slug} for the dashboard "wiki link"
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
type MemoryHandler struct {
	projectsDir    string // absolute path; defaults to ~/.claude/projects
	currentProject string // dirname under projectsDir for the current PWD; empty when not resolvable
	limiter        *ipLimiter
}

// memorySlugRE matches the slug grammar used by Claude's memory system.
// Backslash, dot, slash, and whitespace are excluded so a malicious link
// cannot escape the projects/<scope>/memory/ jail.
var memorySlugRE = regexp.MustCompile(`^[a-zA-Z0-9_\-]{1,64}$`)

// utf8BOM is the byte sequence (EF BB BF) some editors prepend to text files.
// We trim it before parsing so frontmatter detection is not fooled by an
// invisible leading rune. Defined as bytes (not a string literal) to avoid
// embedding the BOM character anywhere inside this Go source file — the
// compiler rejects mid-file BOM.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// memoryLimiter keeps the endpoint cheap to call from a hover handler that
// fires on mouseenter. 10/s burst 20 per IP is comfortable for normal browsing
// and still chokes a runaway script.
const (
	memoryLimiterRate  = 10
	memoryLimiterBurst = 20
)

// errMemoryPathEscape is returned when a slug-derived path resolves outside
// projectsDir despite the regex check. Should never trigger in practice but
// gives the caller something concrete to log if it ever does.
var errMemoryPathEscape = errors.New("path escapes projects dir")

// NewMemoryHandler resolves the projects directory + current project once at
// construction. trustedProxy comes from the auth handler so AllowRequest
// uses the real client IP behind ALB.
func NewMemoryHandler(trustedProxy bool) *MemoryHandler {
	dir := os.Getenv("CLAUDE_PROJECTS_DIR")
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".claude", "projects")
		}
	}
	cur := encodeCurrentProjectDir(dir)
	return &MemoryHandler{
		projectsDir:    dir,
		currentProject: cur,
		limiter:        newIPLimiterWithProxy(memoryLimiterRate, memoryLimiterBurst, trustedProxy),
	}
}

// encodeCurrentProjectDir maps the current working directory to the directory
// name Claude uses under ~/.claude/projects, then verifies the directory exists.
// Returns "" when the project memory dir is missing — in that case we still
// search all other projects.
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

// memoryResponse is the JSON shape returned to the dashboard. Field omission
// rules:
//   - found=false → only {found, slug}
//   - scope="current" → project field omitted
//   - scope="external" → project carries the source project dirname (raw,
//     un-decoded) so the popover can label the origin
type memoryResponse struct {
	Found       bool   `json:"found"`
	Slug        string `json:"slug"`
	Scope       string `json:"scope,omitempty"` // "current" or "external"
	Project     string `json:"project,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"`
	Body        string `json:"body,omitempty"` // raw markdown, frontmatter stripped
}

// handleGet serves GET /api/memory/{slug}.
func (h *MemoryHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	if !h.limiter.AllowRequest(r) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
		return
	}
	slug := r.PathValue("slug")
	if !memorySlugRE.MatchString(slug) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid_slug"})
		return
	}
	if h.projectsDir == "" {
		// Misconfigured environment (no $HOME, no env override). Treat as
		// "not found" rather than 500 so the dashboard still degrades to
		// the broken-link state without surfacing infra noise.
		w.Header().Set("Cache-Control", "private, max-age=30")
		writeJSON(w, memoryResponse{Found: false, Slug: slug})
		return
	}

	resp, err := h.lookup(slug)
	if err != nil {
		// Path-escape is the only error we surface — every other miss is
		// "not found" semantically.
		if errors.Is(err, errMemoryPathEscape) {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid_slug"})
			return
		}
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "io"})
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=30")
	writeJSON(w, resp)
}

// lookup walks the scope chain (current project → other projects) and returns
// the first hit. Returns memoryResponse{Found:false} when nothing matches.
func (h *MemoryHandler) lookup(slug string) (memoryResponse, error) {
	// 1. current project — fast path, single stat.
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

	// 2. other projects — read the projects directory once, skip the
	// current-project entry, sort for determinism, return first hit.
	entries, err := os.ReadDir(h.projectsDir)
	if err != nil {
		// Missing projects dir is "no memories" not 500 (e.g. naozhi running
		// in a fresh box where Claude was never set up).
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
	return memoryResponse{Found: false, Slug: slug}, nil
}

// tryRead loads <projectsDir>/<projectDir>/memory/<slug>.md if it exists.
// Returns (nil, nil) when the file is absent — "no memory in this scope" is
// the common path, not an error.
func (h *MemoryHandler) tryRead(projectDir, slug string) (*memoryResponse, error) {
	full := filepath.Join(h.projectsDir, projectDir, "memory", slug+".md")
	clean := filepath.Clean(full)

	// Defence in depth: even though slug is regex-locked, re-verify the
	// resolved path stays inside projectsDir. Any future change to the
	// slug regex (or projects-dir resolution) will not silently widen the
	// jail.
	prefix := strings.TrimRight(filepath.Clean(h.projectsDir), string(filepath.Separator)) + string(filepath.Separator)
	if !strings.HasPrefix(clean, prefix) {
		return nil, errMemoryPathEscape
	}

	raw, err := os.ReadFile(clean)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	meta, body := parseMemoryFrontmatter(raw)
	resp := &memoryResponse{
		Found:       true,
		Slug:        slug,
		Name:        meta.name,
		Description: meta.description,
		Type:        meta.typ,
		Body:        body,
	}
	return resp, nil
}

// memoryFrontmatter holds the three fields the dashboard popover renders.
// Anything else in the YAML is ignored.
type memoryFrontmatter struct {
	name        string
	description string
	typ         string
}

// parseMemoryFrontmatter strips a leading YAML frontmatter block if present
// and returns the body with leading blank lines trimmed. It is a deliberately
// naive scanner that handles the two layouts the auto-memory system writes:
//
//	---
//	name: foo
//	description: bar
//	metadata:
//	  type: feedback
//	---
//	<body>
//
// Anything that does not match falls through to "no metadata, body = whole
// file". Avoids a YAML dependency for what is otherwise a 5-line schema.
func parseMemoryFrontmatter(raw []byte) (memoryFrontmatter, string) {
	var meta memoryFrontmatter
	// Defensive BOM strip — done on bytes, not on the string, so a literal
	// BOM never appears in this file's source.
	raw = bytes.TrimPrefix(raw, utf8BOM)
	s := string(raw)
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return meta, strings.TrimLeft(s, "\r\n")
	}
	// Cut off the opening fence.
	rest := s
	if strings.HasPrefix(rest, "---\r\n") {
		rest = rest[5:]
	} else {
		rest = rest[4:]
	}
	// Find the closing fence at the start of a line.
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		// Malformed frontmatter — just return the whole file as body.
		return meta, strings.TrimLeft(s, "\r\n")
	}
	front := rest[:idx]
	body := rest[idx+len("\n---"):]
	// Skip the rest of the closing fence line.
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[i+1:]
	} else {
		body = ""
	}

	inMetadata := false
	for _, line := range strings.Split(front, "\n") {
		raw := strings.TrimRight(line, "\r")
		// Track whether we're inside the `metadata:` block by checking the
		// indentation: top-level keys start at column 0; metadata children
		// are indented by 2 spaces (Claude's convention).
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
		// Any non-indented line ends the metadata block.
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

// splitYAMLKV parses a single `key: value` line. Strips matching surrounding
// quotes from the value. Returns ok=false when the line has no colon.
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
