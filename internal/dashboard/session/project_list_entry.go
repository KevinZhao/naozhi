package session

// stats.projects — the per-project rows the dashboard sidebar / palette read
// from /api/sessions. Split out of handlers.go so the wire struct, the
// 1-second local cache and the remote-node merge live together; the row
// mirrors a subset of /api/projects' projectsListEntry (see
// TestProjectListEntry_MirrorsProjectsListEntry).

import (
	"encoding/json"
	"time"

	dashproject "github.com/naozhi/naozhi/internal/dashboard/project"
	"github.com/naozhi/naozhi/internal/project"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
)

// projectListEntry is the per-project element in /api/sessions "stats.projects".
// Named struct (vs map[string]any{6 keys}) eliminates P inner-map allocs and
// 6×P interface{} boxing ops per 1 Hz dashboard poll. `omitempty` tags
// preserve the previous JSON shape: local rows without a git remote, or
// remote-cached rows that didn't round-trip favorite/github, simply drop
// those keys instead of emitting false/"". dashboard.js consumes
// name/path/node/favorite/git_remote_url/github via `p.favorite`, `p.name`,
// etc. — all six are bool-or-string so struct marshaling is byte-equivalent
// to the prior map literal. R70-PERF-M1 / R67-PERF-2 (struct variant).
type projectListEntry struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	Node         string `json:"node"`
	Favorite     bool   `json:"favorite,omitempty"`
	GitRemoteURL string `json:"git_remote_url,omitempty"`
	GitHub       bool   `json:"github,omitempty"`
	// CreatedAt anchors the project's sidebar order: the dashboard sorts
	// projects by this value ascending so newly-added folders always land at
	// the bottom of their tier. unix ms.
	CreatedAt int64 `json:"created_at,omitempty"`

	// The three fields below mirror /api/projects' projectsListEntry byte-
	// for-byte (same names, same tags). dashboard.js populates projectsData
	// ONLY from this stats.projects list, yet reads p.stableKey (continue
	// session), p.dir_mtime (picker order) and p.config.emoji / display_name
	// (labels) — fields that used to exist on /api/projects alone, so the
	// palette silently degraded. TestProjectListEntry_MirrorsProjectsListEntry
	// pins the parity.
	Config     project.ProjectConfig `json:"config"`
	DirModTime int64                 `json:"dir_mtime,omitempty"`
	StableKey  string                `json:"stableKey,omitempty"`
}

// projectListSnapshot caches the local projectList slice build inside
// HandleList at 1-second granularity. Bucket is unix-seconds at the time
// of build; a new bucket triggers a rebuild on the first miss.
//
// READ-ONLY CONTRACT: HandleList reads Entries via the slice header only
// (no append, no element mutation) and copies the header into the response
// struct, which then JSON-encodes into the per-request buffer. Multiple
// concurrent readers therefore alias the same backing array — race-free
// because writers ALWAYS install a freshly built slice, never mutate in
// place. R247-PERF-15 [REPEAT-3].
type projectListSnapshot struct {
	Bucket  int64
	Entries []projectListEntry
}

// projectListLocalAt returns the local projectListEntry slice with 1-second
// cache resolution. The returned slice is shared READ-ONLY across concurrent
// callers in the same bucket; any caller that intends to append must copy
// first (HandleList does this in the remote-merge branch). h.projectMgr
// MUST be non-nil — callers gate on that check before invoking. R247-PERF-15
// [REPEAT-3].
//
// Cache races are benign: two pollers crossing a bucket boundary may each
// rebuild and Store; whichever writes last wins, the loser's locally
// computed slice is GC'd as soon as the response encodes. Critically, both
// rebuilds produce identical content (Manager.All takes a read lock and
// returns sorted snapshots) so observers cannot see torn data even if they
// hold an old header concurrent with the new Store.
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
			// Strip embedded userinfo (PAT) before handing the URL to any
			// dashboard client. Round 46 redacted /api/projects but missed
			// this path — /api/sessions is polled every few seconds, so
			// the leak is actually larger here.
			GitRemoteURL: dashproject.RedactGitRemoteURL(p.GitRemoteURL),
			GitHub:       p.IsGitHub,
		})
	}
	h.projectListCache.Store(&projectListSnapshot{Bucket: bucket, Entries: entries})
	return entries
}

// buildProjectList returns the dashboard sidebar's "Projects" panel data —
// local projects (cached at 1s buckets via projectListLocalAt) plus any
// remote-node projects forwarded through the node cache.
//
// Pre-allocate the outer slice so the append loop doesn't trigger log(N)
// growth reallocs on projects-heavy dashboards. Entries are projectListEntry
// named-struct values (not map[string]any) so the hot 1 Hz poll path skips
// the inner-map + interface{} boxing overhead. R70-PERF-M1.
//
// R247-PERF-15 [REPEAT-3]: collapse N dashboard tabs polling at 1 Hz into
// one rebuild/sec via projectListCache. The 1s bucket is invisible to
// human operators (project CRUD is minute-scale) and avoids touching the
// project package with a version hook. The cached slice is read-only —
// see projectListSnapshot godoc for the alias contract that keeps
// concurrent reads race-free. Split out per R246-CR-002 (#736).
func (h *Handlers) buildProjectList(now time.Time) []projectListEntry {
	var projectList []projectListEntry
	if h.projectMgr != nil {
		projectList = h.projectListLocalAt(now)
	}
	// Merge remote projects (always, even without a local project manager).
	// When we will append remote rows onto the cached local slice we MUST
	// detach the cache first: projectListLocalAt returns the cached header
	// (alias contract), so an append that fits the existing capacity would
	// silently mutate every other reader's view. Building the merged slice
	// fresh keeps the cached entry untouched. R247-PERF-15.
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
			// Remote node may be running an older binary that hasn't
			// redacted the URL yet — always run the redactor on data
			// forwarded via the node cache so credentials never leak
			// even if a peer node is behind on patches.
			if v, ok := item["git_remote_url"].(string); ok && v != "" {
				entry.GitRemoteURL = dashproject.RedactGitRemoteURL(v)
			}
			if v, ok := item["github"].(bool); ok {
				entry.GitHub = v
			}
			// JSON numbers decode as float64 from map[string]any. Pull
			// remote-node CreatedAt the same way; pre-feature peers won't
			// emit the key, so the zero-value fallback keeps their
			// projects at the very top of the sidebar (oldest by
			// definition) until they upgrade and self-stamp.
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
