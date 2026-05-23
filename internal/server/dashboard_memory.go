package server

import (
	"bytes"
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
	projectsDir    string
	currentProject string
	limiter        *ipLimiter
}

var memorySlugRE = regexp.MustCompile(`^[a-zA-Z0-9_\-]{1,64}$`)

// utf8BOM is defined as bytes (not a string literal) so a literal BOM never
// appears in this Go source file — the compiler rejects mid-file BOM.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

const (
	memoryLimiterRate  = 10
	memoryLimiterBurst = 20
)

var errMemoryPathEscape = errors.New("path escapes projects dir")

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
}

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
		w.Header().Set("Cache-Control", "private, max-age=30")
		writeJSON(w, memoryResponse{Found: false, Slug: slug})
		return
	}

	resp, err := h.lookup(slug)
	if err != nil {
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

func (h *MemoryHandler) lookup(slug string) (memoryResponse, error) {
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

func (h *MemoryHandler) tryRead(projectDir, slug string) (*memoryResponse, error) {
	full := filepath.Join(h.projectsDir, projectDir, "memory", slug+".md")
	clean := filepath.Clean(full)

	// Defence in depth: even though slug is regex-locked, re-verify the
	// resolved path stays inside projectsDir.
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
