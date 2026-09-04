package session

// stats.projects — the per-project rows the dashboard sidebar / palette read
// from /api/sessions; the row mirrors a subset of /api/projects'
// projectsListEntry (TestProjectListEntry_MirrorsProjectsListEntry pins it).

import (
	"encoding/json"
	"time"

	dashproject "github.com/naozhi/naozhi/internal/dashboard/project"
	"github.com/naozhi/naozhi/internal/project"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
)

// projectListEntry is the per-project element in /api/sessions
// "stats.projects". omitempty tags keep the wire shape: rows without a git
// remote or without round-tripped favorite/github drop those keys rather than
// emitting false/"". dashboard.js reads name/path/node/favorite/git_remote_url/
// github directly.
type projectListEntry struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	Node         string `json:"node"`
	Favorite     bool   `json:"favorite,omitempty"`
	GitRemoteURL string `json:"git_remote_url,omitempty"`
	GitHub       bool   `json:"github,omitempty"`
	// CreatedAt (unix ms) anchors sidebar order: newly-added folders sort to
	// the bottom of their tier.
	CreatedAt int64 `json:"created_at,omitempty"`

	// Mirror /api/projects' projectsListEntry byte-for-byte: dashboard.js
	// populates projectsData ONLY from stats.projects yet reads p.stableKey,
	// p.dir_mtime and p.config (emoji / display_name).
	// TestProjectListEntry_MirrorsProjectsListEntry pins the parity.
	Config     project.ProjectConfig `json:"config"`
	DirModTime int64                 `json:"dir_mtime,omitempty"`
	StableKey  string                `json:"stableKey,omitempty"`
}

// projectListSnapshot caches the local projectList slice per 1-second bucket
// (unix seconds at build time).
//
// READ-ONLY CONTRACT: HandleList reads Entries via the slice header only and
// copies the header into the response; concurrent readers alias the same
// backing array, which is race-free only because writers ALWAYS install a
// freshly built slice, never mutate in place.
type projectListSnapshot struct {
	Bucket  int64
	Entries []projectListEntry
}

// projectListLocalAt returns the local projectListEntry slice with 1-second
// cache resolution. The result is shared READ-ONLY across callers in the same
// bucket; anyone appending must copy first (buildProjectList does). h.projectMgr
// MUST be non-nil. Cache races are benign: rebuilds across a bucket boundary
// produce identical content (Manager.All returns sorted snapshots under a read
// lock) and last-writer-wins.
func (h *Handlers) projectListLocalAt(now time.Time) []projectListEntry {
	bucket := now.Unix()
	if cur := h.projectListCache.Load(); cur != nil && cur.Bucket == bucket {
		return cur.Entries
	}
	projects := h.projectMgr.All()
	entries := make([]projectListEntry, 0, len(projects))
	for _, p := range projects {
		var stableKey string
		if h.projectStableKeyEnabled {
			stableKey = sessionpkg.ProjectStableKey(p.Path, "general")
		}
		entries = append(entries, projectListEntry{
			Name:       p.Name,
			Path:       p.Path,
			Node:       "local",
			Favorite:   p.Config.Favorite,
			CreatedAt:  p.Config.CreatedAt,
			Config:     p.Config,
			DirModTime: p.DirModTime,
			StableKey:  stableKey,
			// Strip embedded userinfo (PAT) before the URL reaches any dashboard
			// client; /api/sessions is polled constantly so a leak here is worse
			// than on /api/projects.
			GitRemoteURL: dashproject.RedactGitRemoteURL(p.GitRemoteURL),
			GitHub:       p.IsGitHub,
		})
	}
	h.projectListCache.Store(&projectListSnapshot{Bucket: bucket, Entries: entries})
	return entries
}

// buildProjectList returns the sidebar "Projects" panel data: local projects
// (cached per 1s bucket via projectListLocalAt) plus remote-node projects
// forwarded through the node cache. The cached local slice is read-only — see
// projectListSnapshot for the alias contract.
func (h *Handlers) buildProjectList(now time.Time) []projectListEntry {
	var projectList []projectListEntry
	if h.projectMgr != nil {
		projectList = h.projectListLocalAt(now)
	}
	// Appending remote rows MUST NOT touch the cached local slice:
	// projectListLocalAt returns the cached header, so an in-capacity append
	// would silently mutate every other reader's view. Build the merge fresh.
	if !h.nodeAccess.HasNodes() {
		if projectList == nil {
			projectList = []projectListEntry{}
		}
		return projectList
	}
	cachedProjects := h.nodeCache.Projects()
	var remoteCount int
	for _, items := range cachedProjects {
		remoteCount += len(items)
	}
	if remoteCount > 0 {
		merged := make([]projectListEntry, len(projectList), len(projectList)+remoteCount)
		copy(merged, projectList)
		projectList = merged
	}
	for _, items := range cachedProjects {
		for _, item := range items {
			name := strOrFallback(item, "name", "Name")
			path := strOrFallback(item, "path", "Path")
			nd, _ := item["node"].(string)
			if name == "" {
				continue
			}
			entry := projectListEntry{Name: name, Path: path, Node: nd}
			if v, ok := item["favorite"].(bool); ok {
				entry.Favorite = v
			}
			// A peer on an older binary may not have redacted the URL; always run
			// the redactor on node-cache data so credentials never leak.
			if v, ok := item["git_remote_url"].(string); ok && v != "" {
				entry.GitRemoteURL = dashproject.RedactGitRemoteURL(v)
			}
			if v, ok := item["github"].(bool); ok {
				entry.GitHub = v
			}
			// JSON numbers decode as float64. Pre-feature peers omit created_at, so
			// the zero fallback keeps their projects at the top (oldest) until they
			// upgrade.
			if v, ok := item["created_at"].(float64); ok {
				entry.CreatedAt = int64(v)
			}
			if v, ok := item["dir_mtime"].(float64); ok {
				entry.DirModTime = int64(v)
			}
			if v, ok := item["stableKey"].(string); ok {
				entry.StableKey = v
			}
			if v, ok := item["config"].(map[string]any); ok {
				entry.Config = decodeRemoteProjectConfig(v)
			}
			projectList = append(projectList, entry)
		}
	}
	if projectList == nil {
		projectList = []projectListEntry{}
	}
	return projectList
}

// decodeRemoteProjectConfig converts a peer node's `config` object (decoded
// as map[string]any by the node cache) into a ProjectConfig. Unknown keys
// from a newer peer are dropped; a malformed object yields the zero value so
// the row still renders with directory-name fallbacks. Remote-only path —
// the local list copies p.Config directly.
func decodeRemoteProjectConfig(m map[string]any) project.ProjectConfig {
	var cfg project.ProjectConfig
	b, err := json.Marshal(m)
	if err != nil {
		return cfg
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return project.ProjectConfig{}
	}
	return cfg
}
