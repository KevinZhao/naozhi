package project

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// RedactGitRemoteURL strips embedded userinfo (user:password@) from a git
// remote URL before exposing it over the dashboard API: `.git/config` often
// holds https://user:pat@github.com/... from a token clone, and surfacing it
// leaks the PAT to every browser session. SCP-style SSH URLs
// (`git@github.com:org/repo.git`) carry no credentials and pass through
// unchanged rather than being blanked.
func RedactGitRemoteURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		// url.Parse failed or SCP-style (no scheme). SCP URLs carry no
		// credentials, but a "://" string that failed to parse may still embed
		// user:pass@ — strip it defensively.
		if i := strings.Index(raw, "://"); i >= 0 {
			rest := raw[i+3:]
			if at := strings.IndexByte(rest, '@'); at >= 0 {
				// Drop the userinfo segment between "://" and "@".
				return raw[:i+3] + rest[at+1:]
			}
		}
		return raw
	}
	if u.User != nil {
		u.User = nil
	}
	return u.String()
}

// validateProjectName delegates to project.ValidateProjectName, the single
// source of truth for the trust boundary shared by the reverse-RPC worker
// and the dashboard.
func validateProjectName(name string) error {
	return project.ValidateProjectName(name)
}

// Handlers groups the project management API endpoints.
type Handlers struct {
	projectMgr *project.Manager
	router     *session.Router
	// resolver centralises planner-view opts (docs/rfc/key-resolver.md §3.1
	// ResolveForPlannerKey) so planner restart keeps the "no defaults
	// inheritance" contract. Nil falls back to the legacy inlined merge.
	resolver   *session.KeyResolver
	nodeAccess NodeAccessor
	nodeCache  *node.CacheManager
	// baseCtx is the long-lived context the planner-restart timeout derives
	// from; production wires it via SetBaseContext, tests assign directly.
	// Nil falls back to Background in restartCtx (#650).
	baseCtx context.Context
	// filesExistsLimiter caps /api/projects/files/exists per caller: the
	// endpoint fans out up to maxExistsPaths stats within fileStatTimeout, so
	// unmetered calls let a post-auth attacker tie up workers on slow trees.
	// Mirrors the uploadLimiter policy (≈10/min). Nil in hand-built test
	// Handlers; HandleFilesExists nil-guards.
	filesExistsLimiter IPLimiter
	// configPutLimiter caps PUT /api/projects/config per IP: each write hits
	// disk and fans out a WS update to every dashboard client. 5/sec burst 5
	// is far above interactive saves but below abuse rates. Nil-safe in tests.
	configPutLimiter IPLimiter
	// uploadQuota bounds cumulative upload bytes per project within this
	// process (#2311). Nil disables enforcement (single-operator model / tests).
	uploadQuota *uploadQuota
	// publicTmpEnabled gates the __public_tmp__ pseudo-project (#646). When
	// false (default) it is rejected as "project not found" like any missing
	// project, so multi-user deployments cannot enumerate / preview arbitrary
	// /tmp paths merely because the naozhi process has DAC read access.
	// Single-operator setups flip it via server.public_tmp_enabled.
	publicTmpEnabled bool
	// projectStableKeyEnabled gates emitting StableKey in the list response
	// (docs/rfc/project-stable-session-key.md §4.2); when false the frontend
	// falls back to the legacy timestamp-key path for "continue". Default true.
	projectStableKeyEnabled bool
}

// Deps bundles all wiring for New so internal/server can construct Handlers
// without access to unexported fields.
type Deps struct {
	ProjectMgr         *project.Manager
	Router             *session.Router
	Resolver           *session.KeyResolver
	NodeAccess         NodeAccessor
	NodeCache          *node.CacheManager
	FilesExistsLimiter IPLimiter
	ConfigPutLimiter   IPLimiter
	PublicTmpEnabled   bool
	// UploadQuotaBytes caps cumulative per-project upload bytes within a
	// process (#2311). <=0 disables enforcement.
	UploadQuotaBytes int64
	// ProjectStableKeyEnabled toggles the StableKey field in the list
	// response. Production wires cfg.Session.ProjectStableKey.ResolvedEnabled(true).
	ProjectStableKeyEnabled bool
}

// New constructs a Handlers from injected deps.
func New(d Deps) *Handlers {
	return &Handlers{
		projectMgr:         d.ProjectMgr,
		router:             d.Router,
		resolver:           d.Resolver,
		nodeAccess:         d.NodeAccess,
		nodeCache:          d.NodeCache,
		filesExistsLimiter: d.FilesExistsLimiter,
		configPutLimiter:   d.ConfigPutLimiter,
		uploadQuota:        newUploadQuota(d.UploadQuotaBytes),
		publicTmpEnabled:   d.PublicTmpEnabled,

		projectStableKeyEnabled: d.ProjectStableKeyEnabled,
	}
}

// SetBaseContext wires the long-lived process context (typically Hub.ctx)
// used by the planner-restart timeout; tests may assign h.baseCtx directly (#650).
func (h *Handlers) SetBaseContext(ctx context.Context) {
	h.baseCtx = ctx
}

// restartCtx returns the parent context for handleRestartPlanner's 30s
// timeout, falling back to context.Background() when baseCtx is unwired.
func (h *Handlers) restartCtx() context.Context {
	if h.baseCtx != nil {
		return h.baseCtx
	}
	return context.Background()
}

// projectsListEntry is the per-project element in GET /api/projects. The
// JSON shape is pinned by TestDashboardJSON_Projects_ShapeContract:
// `git_remote_url` and `github` must always be present (the dashboard JS
// reads them unconditionally), so neither carries omitempty. `Node` keeps
// omitempty because the local-only path never sets it.
type projectsListEntry struct {
	Name         string                `json:"name"`
	Path         string                `json:"path"`
	Node         string                `json:"node,omitempty"`
	PlannerState string                `json:"planner_state"`
	PlannerModel string                `json:"planner_model"`
	Config       project.ProjectConfig `json:"config"`
	Favorite     bool                  `json:"favorite"`
	GitRemoteURL string                `json:"git_remote_url"`
	GitHub       bool                  `json:"github"`
	// IsRoot marks the synthetic projects-root entry (include_root). The
	// files view defaults its browse root to this project so the operator
	// lands at the workspace root rather than the first subdirectory project.
	IsRoot bool `json:"is_root,omitempty"`
	// DirModTime is the project directory's mtime (unix ms) from Manager.Scan;
	// the "new session" picker orders its fallback tier by it descending.
	// omitempty: the JS reads `p.dir_mtime || 0`, so absence is handled.
	DirModTime int64 `json:"dir_mtime,omitempty"`
	// StableKey is the project-level stable dashboard session key for the
	// general agent: dashboard:pj:<workspace-hash>:general
	// (docs/rfc/project-stable-session-key.md §4.2). The backend is the SOLE
	// owner of the workspace hash so the frontend never re-implements sha256;
	// for another agent it swaps the trailing :general segment. Empty for
	// remote/path-less entries.
	StableKey string `json:"stableKey,omitempty"`
}

// ProjectsListEntryType exposes the /api/projects row type for cross-package
// wire-shape contract tests (the /api/sessions stats.projects row mirrors a
// subset of its fields and must not drift).
func ProjectsListEntryType() reflect.Type { return reflect.TypeOf(projectsListEntry{}) }

// HandleList serves GET /api/projects — list all projects (local + remote).
func (h *Handlers) HandleList(w http.ResponseWriter, r *http.Request) {
	if h.projectMgr == nil {
		httputil.WriteJSON(w, []any{})
		return
	}

	projects := h.projectMgr.All()
	result := make([]projectsListEntry, 0, len(projects))
	for _, p := range projects {
		plannerKey := p.PlannerSessionKey()
		plannerState := "none"
		if sess := h.router.SessionFor(plannerKey); sess != nil {
			snap := sess.Snapshot()
			plannerState = snap.State
		}

		var stableKey string
		if h.projectStableKeyEnabled {
			stableKey = session.ProjectStableKey(p.Path, "general")
		}

		result = append(result, projectsListEntry{
			Name:         p.Name,
			Path:         p.Path,
			PlannerState: plannerState,
			PlannerModel: h.projectMgr.EffectivePlannerModel(p),
			Config:       p.Config,
			Favorite:     p.Config.Favorite,
			GitRemoteURL: RedactGitRemoteURL(p.GitRemoteURL),
			GitHub:       p.IsGitHub,
			IsRoot:       p.IsRoot,
			DirModTime:   p.DirModTime,
			StableKey:    stableKey,
		})
	}

	// Merge remote projects
	if h.nodeAccess.HasNodes() {
		allProjects := make([]any, 0, len(result))
		for i := range result {
			result[i].Node = "local"
			allProjects = append(allProjects, result[i])
		}
		cachedProjects := h.nodeCache.Projects()
		for _, items := range cachedProjects {
			for _, item := range items {
				allProjects = append(allProjects, item)
			}
		}
		httputil.WriteJSON(w, allProjects)
		return
	}

	httputil.WriteJSON(w, result)
}

// HandleConfigGet serves GET /api/projects/config?name=...
func (h *Handlers) HandleConfigGet(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if err := validateProjectName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if h.projectMgr == nil {
		http.Error(w, "projects not configured", http.StatusBadRequest)
		return
	}

	p := h.projectMgr.Get(name)
	if p == nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}

	httputil.WriteJSON(w, p.Config)
}

// HandleConfigPut serves PUT /api/projects/config?name=...
func (h *Handlers) HandleConfigPut(w http.ResponseWriter, r *http.Request) {
	// Rate-limit before any work: disk write + dashboard-wide WS fan-out is
	// otherwise unmetered for any authenticated caller. Nil-guarded for tests.
	if h.configPutLimiter != nil && !h.configPutLimiter.AllowRequest(r) {
		w.Header().Set("Retry-After", "1")
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "config update rate limit exceeded"})
		return
	}
	name := r.URL.Query().Get("name")
	if err := validateProjectName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Cap body size before either the remote-proxy read or the local decode
	// so a remote proxy cannot smuggle a larger body than the local handler
	// accepts; 64 KB is well above legitimate configs.
	r = httputil.WithMaxBytes(w, r, 64*1024)

	// Remote node proxy
	nodeID := r.URL.Query().Get("node")
	if nodeID != "" && nodeID != "local" {
		nc, ok := h.nodeAccess.LookupNode(w, nodeID)
		if !ok {
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body failed", http.StatusBadRequest)
			return
		}
		if err := nc.ProxyUpdateConfig(r.Context(), name, body); err != nil {
			slog.Warn("proxy update config failed", "node", nodeID, "err", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		httputil.WriteOK(w)
		return
	}

	if h.projectMgr == nil {
		http.Error(w, "projects not configured", http.StatusBadRequest)
		return
	}

	var cfg project.ProjectConfig
	if err := httputil.DecodeJSONBody(r, &cfg); err != nil {
		// Fixed error string: echoing err.Error() leaks the decoder's field
		// names / offsets which help schema enumeration.
		slog.Debug("project config: decode failed", "err", err, "project", name)
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if err := project.ValidateConfig(cfg); err != nil {
		// ValidateConfig's field-specific messages echo internal size caps;
		// log the detail for the operator but keep the HTTP response generic.
		slog.Debug("project update_config: ValidateConfig failed", "project", name, "err", err)
		http.Error(w, "invalid project config", http.StatusBadRequest)
		return
	}

	if err := h.projectMgr.UpdateConfig(name, cfg); err != nil {
		if errors.Is(err, project.ErrNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
		} else {
			slog.Error("update project config failed", "project", name, "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
		return
	}

	httputil.WriteOK(w)
}

// HandleFavoriteToggle serves POST /api/projects/favorite?name=...&favorite=true|false
func (h *Handlers) HandleFavoriteToggle(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if err := validateProjectName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	favStr := r.URL.Query().Get("favorite")
	if favStr != "true" && favStr != "false" {
		http.Error(w, "favorite must be true or false", http.StatusBadRequest)
		return
	}
	favorite := favStr == "true"

	// Remote node proxy
	nodeID := r.URL.Query().Get("node")
	if nodeID != "" && nodeID != "local" {
		nc, ok := h.nodeAccess.LookupNode(w, nodeID)
		if !ok {
			return
		}
		if err := nc.ProxySetFavorite(r.Context(), name, favorite); err != nil {
			slog.Warn("proxy set favorite failed", "node", nodeID, "err", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		// Bump local version so the dashboard's version gate doesn't skip the
		// next /api/sessions poll while the remote's favorite change is still
		// propagating into our nodeCache.
		if h.router != nil {
			h.router.BumpVersion()
		}
		httputil.WriteJSON(w, map[string]any{"status": "ok", "favorite": favorite})
		return
	}

	if h.projectMgr == nil {
		http.Error(w, "projects not configured", http.StatusBadRequest)
		return
	}
	if err := h.projectMgr.SetFavorite(name, favorite); err != nil {
		if errors.Is(err, project.ErrNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
		} else {
			slog.Error("set favorite failed", "project", name, "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
		return
	}
	// Bump the router version so the dashboard's version-gated fetchSessions()
	// notices the favorite flip instead of waiting for the next session event.
	if h.router != nil {
		h.router.BumpVersion()
	}
	httputil.WriteJSON(w, map[string]any{"status": "ok", "favorite": favorite})
}

// HandlePlannerRestart serves POST /api/projects/planner/restart?name=...
func (h *Handlers) HandlePlannerRestart(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if err := validateProjectName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Remote node proxy
	nodeID := r.URL.Query().Get("node")
	if nodeID != "" && nodeID != "local" {
		nc, ok := h.nodeAccess.LookupNode(w, nodeID)
		if !ok {
			return
		}
		if err := nc.ProxyRestartPlanner(r.Context(), name); err != nil {
			slog.Warn("proxy restart planner failed", "node", nodeID, "err", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		httputil.WriteJSON(w, map[string]string{"status": "restarting"})
		return
	}

	if h.projectMgr == nil {
		http.Error(w, "projects not configured", http.StatusBadRequest)
		return
	}

	// Derive planner-view opts via the resolver (ResolveForPlannerKey), which
	// keeps the "do not read defaults" contract (docs/rfc/key-resolver.md
	// §2.2 #6). Legacy fallback serves headless test paths without a resolver.
	var plannerKey string
	var opts session.AgentOpts
	if h.resolver != nil {
		key, plannerOpts, ok := h.resolver.ResolveForPlannerKey(name)
		if !ok {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		plannerKey = key
		opts = plannerOpts
	} else {
		p := h.projectMgr.Get(name)
		if p == nil {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		plannerKey = p.PlannerSessionKey()
		opts = session.AgentOpts{
			Model:     h.projectMgr.EffectivePlannerModel(p),
			Workspace: p.Path,
			Exempt:    true,
		}
		// Spawn-boundary re-validation (#535): EffectivePlannerPrompt re-reads
		// cached project.yaml / CLAUDE.md, which Claude's Write tool can mutate
		// past ValidateConfig. Drop the prompt entirely when sanitisation fails
		// rather than feeding control bytes / oversize argv to the CLI.
		if pp := session.SanitisePlannerPromptForSpawn(h.projectMgr.EffectivePlannerPrompt(p), p.Name); pp != "" {
			opts.SystemPrompt = pp // #2493: dedicated field, not ExtraArgs
		}
	}

	ctx, cancel := context.WithTimeout(h.restartCtx(), 30*time.Second)
	defer cancel()
	if _, err := h.router.ResetAndRecreate(ctx, plannerKey, opts); err != nil {
		slog.Error("planner restart failed", "project", name, "err", err)
		http.Error(w, "restart failed", http.StatusInternalServerError)
		return
	}
	slog.Info("planner restarted", "project", name)

	httputil.WriteJSON(w, map[string]string{"status": "restarted"})
}

// HasFilesExistsLimiter reports whether FilesExistsLimiter has been wired;
// exposed for the server-package test pinning that server.New wires it.
func (h *Handlers) HasFilesExistsLimiter() bool {
	return h.filesExistsLimiter != nil
}
