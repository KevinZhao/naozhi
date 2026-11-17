package memory

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	dashproject "github.com/naozhi/naozhi/internal/dashboard/project"
	"github.com/naozhi/naozhi/internal/textutil"
)

// GET /api/memories: the settings view's 懂你 section (planner RFC §15.15 ⑦,
// D126) — every project's auto-memory on this node, grouped by project, the
// scope the Planner reads (agent.go allowRules). Nothing is cached: each
// open reads the disk again, since the CLI's sessions write these files
// while the page is open.

// Group kinds, in the order the section lists them.
const (
	kindPlanner   = "planner"
	kindWorkspace = "workspace"
	kindOther     = "other"
	kindTemp      = "temp"
)

var kindRank = map[string]int{kindPlanner: 0, kindWorkspace: 1, kindOther: 2, kindTemp: 3}

// tempPrefixes are the encoded directories of throwaway working directories
// (isolated instances, test runs): listed last and folded.
var tempPrefixes = []string{"-private-tmp-", "-tmp-", "-private-var-folders-", "-var-folders-"}

// Read-only reasons an entry carries; "" is editable.
const (
	roTooLarge = "too_large"
	roNotUTF8  = "not_utf8"
	// roOddName is a file whose stem is not a name parameter can carry: it
	// is listed (the Planner reads it) but opened in an editor only.
	roOddName = "odd_name"
)

// snippetRunes caps an entry's first paragraph, which the page filters on.
const snippetRunes = 200

// indexLinkRE finds the memory files an index line links, "(<name>.md)".
var indexLinkRE = regexp.MustCompile(`\(([^()\s/\\]+)\.md\)`)

type listEntry struct {
	Name         string    `json:"name"`
	Title        string    `json:"title"`
	Type         string    `json:"type,omitempty"`
	Description  string    `json:"description,omitempty"`
	Snippet      string    `json:"snippet,omitempty"`
	Source       string    `json:"source,omitempty"`
	RememberedAt string    `json:"remembered_at,omitempty"`
	Modified     time.Time `json:"modified"`
	Size         int64     `json:"size"`
	// Version is what a delete from the list names (file.go version).
	Version  string `json:"version"`
	Indexed  bool   `json:"indexed"`
	ReadOnly string `json:"readonly,omitempty"`
}

type listIndex struct {
	Lines    int       `json:"lines"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	ReadOnly string    `json:"readonly,omitempty"`
}

type listGroup struct {
	// Project is the directory under the projects root, the project
	// parameter of the file routes.
	Project string `json:"project"`
	Label   string `json:"label"`
	Kind    string `json:"kind"`
	// WorkspaceID / WorkspaceName are the registered workspace the group
	// is ("global" for the Planner's own), for the 记下了 card to find it.
	WorkspaceID   string    `json:"workspace_id,omitempty"`
	WorkspaceName string    `json:"workspace_name,omitempty"`
	Modified      time.Time `json:"modified"`
	// Index is the project's MEMORY.md; nil without one.
	Index *listIndex `json:"index,omitempty"`
	// Dangling counts the index lines linking a file that is not there.
	Dangling int         `json:"dangling,omitempty"`
	Entries  []listEntry `json:"entries"`
}

type claudeMDInfo struct {
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

type listResponse struct {
	// Writable is whether this request may edit and delete: only one that
	// reached this node's own listener from a loopback address
	// (directLoopback). The page shows 只读 otherwise.
	Writable bool          `json:"writable"`
	Groups   []listGroup   `json:"groups"`
	ClaudeMD *claudeMDInfo `json:"claude_md,omitempty"`
}

// HandleList serves GET /api/memories.
func (h *Handler) HandleList(w http.ResponseWriter, r *http.Request) {
	resp := listResponse{Writable: directLoopback(r), Groups: []listGroup{}}
	if h.claudeMD != "" {
		if st, err := os.Stat(h.claudeMD); err == nil && st.Mode().IsRegular() {
			resp.ClaudeMD = &claudeMDInfo{Size: st.Size(), Modified: st.ModTime()}
		}
	}
	if h.projectsDir != "" {
		groups, err := h.listGroups()
		if err != nil {
			httputil.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "io"})
			return
		}
		resp.Groups = groups
	}
	w.Header().Set("Cache-Control", "no-store")
	httputil.WriteJSON(w, resp)
}

func (h *Handler) listGroups() ([]listGroup, error) {
	dirs, err := os.ReadDir(h.projectsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []listGroup{}, nil
		}
		return nil, err
	}
	var ws []Workspace
	if h.workspaces != nil {
		ws = h.workspaces()
	}
	out := []listGroup{}
	// byWorkspace counts the groups each workspace names, so two directories
	// that both match one (Linux, two case spellings) are told apart.
	byWorkspace := map[string][]int{}
	for _, d := range dirs {
		project := d.Name()
		if !validProject(project) {
			continue
		}
		g, ok := h.readGroup(project)
		if !ok {
			continue
		}
		switch {
		case h.plannerOwn(project):
			g.Kind, g.Label, g.WorkspaceID = kindPlanner, "Planner 自己的", "global"
		default:
			if w, ok := matchWorkspace(project, ws); ok {
				g.Kind, g.Label, g.WorkspaceID, g.WorkspaceName = kindWorkspace, w.Name, w.ID, w.Name
				byWorkspace[w.ID+"\x00"+w.Name] = append(byWorkspace[w.ID+"\x00"+w.Name], len(out))
			} else if isTemp(project) {
				g.Kind, g.Label = kindTemp, project
			} else {
				g.Kind = kindOther
				if cwd := h.transcriptCwd(project); cwd != "" {
					g.Label = cwd
				} else {
					g.Label = project
				}
			}
		}
		out = append(out, g)
	}
	for _, idx := range byWorkspace {
		if len(idx) < 2 {
			continue
		}
		for _, i := range idx {
			out[i].Label += " · " + out[i].Project
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ri, rj := kindRank[out[i].Kind], kindRank[out[j].Kind]; ri != rj {
			return ri < rj
		}
		if !out[i].Modified.Equal(out[j].Modified) {
			return out[i].Modified.After(out[j].Modified)
		}
		return out[i].Project < out[j].Project
	})
	return out, nil
}

// matchWorkspace finds the registered workspace whose directory the CLI
// encodes as project. Case is ignored: macOS reaches one directory by
// several case spellings (a registered /Users/x/Workspace and sessions run
// in /Users/x/workspace), and the CLI encodes the spelling it was run in.
func matchWorkspace(project string, ws []Workspace) (Workspace, bool) {
	for _, w := range ws {
		if w.Path != "" && strings.EqualFold(claudefs.ProjectSlug(w.Path), project) {
			return w, true
		}
	}
	return Workspace{}, false
}

func isTemp(project string) bool {
	for _, p := range tempPrefixes {
		if strings.HasPrefix(project, p) {
			return true
		}
	}
	return false
}

// readGroup reads project's memory directory; ok is false for one with no
// memory directory, or with neither an entry nor an index (most are: the
// CLI makes the directory for every project it runs in).
func (h *Handler) readGroup(project string) (listGroup, bool) {
	g := listGroup{Project: project, Entries: []listEntry{}}
	dir, err := h.memoryDir(project)
	if err != nil {
		return g, false
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return g, false
	}
	linked := map[string]bool{}
	for _, f := range files {
		name := f.Name()
		if !strings.HasSuffix(name, ".md") || f.IsDir() {
			continue
		}
		stem := strings.TrimSuffix(name, ".md")
		if strings.EqualFold(name, claudefs.MemoryIndexFile) {
			if idx, links, ok := h.readIndex(dir, name); ok {
				g.Index = idx
				linked = links
				if idx.Modified.After(g.Modified) {
					g.Modified = idx.Modified
				}
			}
			continue
		}
		e, ok := h.readEntry(dir, name)
		if !ok {
			continue
		}
		e.Name = stem
		if !memorySlugRE.MatchString(stem) {
			e.ReadOnly = roOddName
		}
		g.Entries = append(g.Entries, e)
		if e.Modified.After(g.Modified) {
			g.Modified = e.Modified
		}
	}
	if len(g.Entries) == 0 && g.Index == nil {
		return g, false
	}
	present := map[string]bool{}
	for i := range g.Entries {
		present[g.Entries[i].Name] = true
		g.Entries[i].Indexed = linked[g.Entries[i].Name]
	}
	for name := range linked {
		if !present[name] {
			g.Dangling++
		}
	}
	sort.SliceStable(g.Entries, func(i, j int) bool {
		if !g.Entries[i].Modified.Equal(g.Entries[j].Modified) {
			return g.Entries[i].Modified.After(g.Entries[j].Modified)
		}
		return g.Entries[i].Name < g.Entries[j].Name
	})
	return g, true
}

func (h *Handler) readEntry(dir, base string) (listEntry, bool) {
	path, err := h.resolveIn(dir, base)
	if err != nil {
		return listEntry{}, false
	}
	raw, truncated, err := readCappedMemoryFile(path, maxMemoryFileBytes)
	if err != nil {
		return listEntry{}, false
	}
	st, err := os.Stat(path)
	if err != nil {
		return listEntry{}, false
	}
	e := listEntry{Modified: st.ModTime(), Size: st.Size(), Version: version(st)}
	switch {
	case truncated:
		e.ReadOnly = roTooLarge
	case !utf8.Valid(raw):
		e.ReadOnly = roNotUTF8
	}
	meta, body := parseMemoryFrontmatter([]byte(strings.ToValidUTF8(string(raw), "�")))
	e.Title = sanitizeWireText(meta.name)
	if e.Title == "" {
		e.Title = sanitizeWireText(strings.TrimSuffix(base, ".md"))
	}
	e.Type = sanitizeWireText(meta.typ)
	e.Description = sanitizeWireText(meta.description)
	e.Source = sanitizeWireText(meta.source)
	e.RememberedAt = sanitizeWireText(meta.rememberedAt)
	e.Snippet = sanitizeWireText(firstParagraph(body))
	return e, true
}

// readIndex reads a project's MEMORY.md: its size and lines, and the names
// its lines link.
func (h *Handler) readIndex(dir, base string) (*listIndex, map[string]bool, bool) {
	path, err := h.resolveIn(dir, base)
	if err != nil {
		return nil, nil, false
	}
	raw, truncated, err := readCappedMemoryFile(path, maxMemoryFileBytes)
	if err != nil {
		return nil, nil, false
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, nil, false
	}
	idx := &listIndex{Size: st.Size(), Modified: st.ModTime()}
	switch {
	case truncated:
		idx.ReadOnly = roTooLarge
	case !utf8.Valid(raw):
		idx.ReadOnly = roNotUTF8
	}
	links := map[string]bool{}
	text := strings.TrimRight(string(raw), "\n")
	if text != "" {
		lines := strings.Split(text, "\n")
		idx.Lines = len(lines)
		for _, l := range lines {
			for _, m := range indexLinkRE.FindAllStringSubmatch(l, -1) {
				links[m[1]] = true
			}
		}
	}
	return idx, links, true
}

// firstParagraph is body's first non-empty block of lines, one line.
func firstParagraph(body string) string {
	var lines []string
	for _, l := range strings.Split(body, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			if len(lines) > 0 {
				break
			}
			continue
		}
		lines = append(lines, l)
	}
	return textutil.TruncateRunes(strings.Join(lines, " "), snippetRunes)
}

// maxCwdLines and maxCwdBytes bound the transcript read for a group's name.
const (
	maxCwdLines = 16
	maxCwdBytes = 256 * 1024
	// maxCwdScan bounds the directory entries looked through for a
	// transcript; a project holds thousands.
	maxCwdScan = 512
)

// transcriptCwd is the working directory a transcript of project names
// (the `cwd` of one of its first records): the name of a group no
// registered workspace names. The directory name itself is not decoded
// back to a path: the encoding is lossy (every non-alphanumeric byte is
// '-', long paths are hashed). "" when none says.
func (h *Handler) transcriptCwd(project string) string {
	dir := filepath.Join(h.projectsDir, project)
	f, err := os.Open(dir)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanned := 0
	for scanned < maxCwdScan {
		ents, err := f.ReadDir(64)
		for _, e := range ents {
			scanned++
			if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			if cwd := firstCwd(filepath.Join(dir, e.Name())); cwd != "" {
				return sanitizeWireText(cwd)
			}
		}
		if err != nil || len(ents) == 0 {
			return ""
		}
	}
	return ""
}

func firstCwd(path string) string {
	f, err := dashproject.OpenWorkspaceFile(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, maxCwdBytes))
	sc.Buffer(make([]byte, 0, 64*1024), maxCwdBytes)
	for i := 0; i < maxCwdLines && sc.Scan(); i++ {
		var rec struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil && filepath.IsAbs(rec.Cwd) {
			return rec.Cwd
		}
	}
	return ""
}
