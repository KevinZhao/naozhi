package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// redactGitRemoteURL strips embedded userinfo (user:password@) from a git
// remote URL before exposing it over the dashboard API. `.git/config` often
// stores credentials like https://user:pat@github.com/org/repo when the user
// originally cloned with a token; surfacing that verbatim in JSON responses
// leaks the PAT to any browser session.
//
// SCP-style SSH URLs (`git@github.com:org/repo.git`) carry no credentials
// and parse as relative paths (no Scheme) under url.Parse — they pass through
// unchanged. Returning "" for those would make the dashboard lose the clone
// URL for every SSH-cloned project, which is a worse failure mode than the
// theoretical "userinfo in an unparseable form" edge case.
func redactGitRemoteURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return raw
	}
	if u.User != nil {
		u.User = nil
	}
	return u.String()
}

// maxProjectNameLen bounds the `name` query param on project endpoints.
// Kept as an alias of project.MaxProjectNameBytes so existing tests /
// callers compile unchanged; the two constants were always required to
// stay in lockstep. R183-REFACTOR-L1.
const maxProjectNameLen = project.MaxProjectNameBytes

// validateProjectName is a thin wrapper over project.ValidateProjectName.
//
// Previously this file carried a full duplicate of the policy with the
// same 128-byte cap, C0/DEL gate, and IsLogInjectionRune sweep. Keeping
// two validators in sync across code review rounds was error-prone —
// R181-SEC-P2-2 explicitly introduced project.ValidateProjectName as
// the single source of truth for trust boundaries (reverse-RPC worker +
// dashboard). R183-REFACTOR-L1 collapses the dashboard path onto the
// same function so a future tightening (e.g. rejecting leading hyphens
// to block flag injection through filesystem lookups) needs one edit.
func validateProjectName(name string) error {
	return project.ValidateProjectName(name)
}

// ProjectHandlers groups the project management API endpoints.
type ProjectHandlers struct {
	projectMgr *project.Manager
	router     *session.Router
	// resolver centralises planner-view opts (docs/rfc/key-resolver.md
	// §3.1 ResolveForPlannerKey). Used by planner restart to avoid
	// re-implementing the "no defaults inheritance" contract that
	// distinguishes administrative planner restarts from chat-view
	// session spawns. Nil falls back to the legacy inlined merge.
	resolver   *session.KeyResolver
	nodeAccess NodeAccessor
	nodeCache  *node.CacheManager
	// baseCtx is the long-lived context the planner-restart timeout
	// derives from. R247-ARCH-15 (#650): replaces a `ctxFunc func()
	// context.Context` closure that captured `*Server` indirectly
	// through `s.hub.ctx`. The closure-as-DI antipattern made test
	// substitution awkward — every test had to rebuild a closure. Now
	// tests assign `h.baseCtx = ctx` directly. Production wiring
	// happens via `SetBaseContext` from registerDashboard once
	// `s.hub.ctx` exists. Nil falls back to Background in restartCtx
	// so a handler cannot panic on an un-wired ProjectHandlers
	// (matches the prior closure's `Background()` fallback).
	baseCtx context.Context
	// filesExistsLimiter caps how often a single authenticated caller can
	// invoke /api/projects/files/exists. The endpoint fans out up to
	// maxExistsPaths (100) filesystem stats per request with a
	// fileStatTimeout (2s) budget, so unmetered calls let a post-auth
	// attacker tie up worker goroutines against slow/deep directory trees
	// (NFS mounts, gigantic monorepos, symlink loops). Mirrors the
	// uploadLimiter policy (6s period × 10 burst ≈ 10/min) since both
	// endpoints do filesystem I/O and belong to the same DoS class.
	// Nil in tests that build ProjectHandlers by hand; handleFilesExists
	// guards with a nil check so the limiter is optional. S13.
	filesExistsLimiter *ipLimiter
	// configPutLimiter caps PUT /api/projects/config writes per IP. The
	// handler persists ProjectConfig to disk and broadcasts a WS update
	// fan-out to every subscribed dashboard client; an unmetered post-auth
	// caller can therefore drive disk I/O + N×WS fan-out at line rate.
	// 5/sec burst 5 ≈ 5×60=300/min — well above interactive UX (a human
	// only saves config sub-second after edit) but below abuse rates.
	// Nil-safe in tests; handleConfigPut guards with a nil check.
	// R247-SEC-7.
	configPutLimiter *ipLimiter
	// publicTmpEnabled gates the __public_tmp__ pseudo-project (R237-SEC-5,
	// #646). When false (default), any request naming publicTmpProject as
	// the project field is rejected as "project not found" — same surface
	// as a non-existent regular project. The single-operator dashboard
	// model can flip this to true via server.public_tmp_enabled in
	// config.yaml; multi-user deployments leave it off so an authenticated
	// dashboard user cannot enumerate / preview arbitrary /tmp paths
	// (other operators' editor swaps, systemd-private payloads, …) just
	// because the naozhi process happens to have read access on Linux DAC.
	publicTmpEnabled bool
}

// SetBaseContext wires the long-lived process context (typically
// `Hub.ctx`) used by the planner-restart timeout. Called from
// registerDashboard after the Hub is constructed. Pre-wiring tests
// can assign `h.baseCtx` directly; this setter exists so production
// callers don't need to reach into an unexported field. R247-ARCH-15
// (#650).
func (h *ProjectHandlers) SetBaseContext(ctx context.Context) {
	h.baseCtx = ctx
}

// restartCtx returns the parent context for `handleRestartPlanner`'s
// 30s timeout. Falls back to context.Background() when baseCtx has
// not been wired (test paths that build ProjectHandlers by hand
// without calling SetBaseContext). Mirrors the prior `ctxFunc`
// closure's nil branch so behaviour is unchanged for existing call
// sites. R247-ARCH-15 (#650).
func (h *ProjectHandlers) restartCtx() context.Context {
	if h.baseCtx != nil {
		return h.baseCtx
	}
	return context.Background()
}

// projectsListEntry is the per-project element in GET /api/projects.
// R247-PERF-6: replaces the prior `map[string]any{8 keys}` literal that
// allocated one inner map + 8 interface{} boxing slots per project per
// dashboard poll. The JSON shape pinned by TestDashboardJSON_Projects_
// ShapeContract requires `git_remote_url` and `github` to always be
// present (the dashboard JS reads them unconditionally), so neither
// carries `omitempty` even though the empty/false zero values were
// previously emitted via the map literal too. `Node` keeps `omitempty`
// because the local-only path never sets it; the multi-node merge path
// stamps "local" before serialise.
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
}

// GET /api/projects — list all projects (local + remote).
func (h *ProjectHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	if h.projectMgr == nil {
		writeJSON(w, []any{})
		return
	}

	projects := h.projectMgr.All()
	result := make([]projectsListEntry, 0, len(projects))
	for _, p := range projects {
		plannerKey := p.PlannerSessionKey()
		plannerState := "none"
		if sess := h.router.GetSession(plannerKey); sess != nil {
			snap := sess.Snapshot()
			plannerState = snap.State
		}

		result = append(result, projectsListEntry{
			Name:         p.Name,
			Path:         p.Path,
			PlannerState: plannerState,
			PlannerModel: h.projectMgr.EffectivePlannerModel(p),
			Config:       p.Config,
			Favorite:     p.Config.Favorite,
			GitRemoteURL: redactGitRemoteURL(p.GitRemoteURL),
			GitHub:       p.IsGitHub,
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
		writeJSON(w, allProjects)
		return
	}

	writeJSON(w, result)
}

// GET /api/projects/config?name=...
func (h *ProjectHandlers) handleConfigGet(w http.ResponseWriter, r *http.Request) {
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

	writeJSON(w, p.Config)
}

// PUT /api/projects/config?name=...
func (h *ProjectHandlers) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	// R247-SEC-7: cap per-IP rate before any work — disk write +
	// dashboard-wide WS fan-out is otherwise rate-unlimited for any
	// authenticated caller. Same nil-guard convention as filesExistsLimiter
	// so tests that build ProjectHandlers by hand still pass.
	if h.configPutLimiter != nil && !h.configPutLimiter.AllowRequest(r) {
		w.Header().Set("Retry-After", "1")
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "config update rate limit exceeded"})
		return
	}
	name := r.URL.Query().Get("name")
	if err := validateProjectName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Cap incoming body size before either the remote-proxy read or the
	// local JSON decode. Project configs are small (schedule + planner
	// prompt); 64 KB is well above legitimate payloads and keeps both
	// paths consistent so a remote proxy cannot be used to smuggle a
	// larger body than the local handler would accept.
	r = withMaxBytes(w, r, 64*1024)

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
		writeOK(w)
		return
	}

	if h.projectMgr == nil {
		http.Error(w, "projects not configured", http.StatusBadRequest)
		return
	}

	var cfg project.ProjectConfig
	if err := decodeJSONBody(r, &cfg); err != nil {
		// Fixed error string: echoing err.Error() leaks the decoder's field
		// names / offsets which help schema enumeration.
		slog.Debug("project config: decode failed", "err", err, "project", name)
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if err := project.ValidateConfig(cfg); err != nil {
		// R181-SEC-P2-5: ValidateConfig returns field-specific strings
		// like "planner_prompt exceeds 8192-byte limit" that echo the
		// internal size caps — a low-risk information leak to
		// authenticated users probing config field names. The wrapped
		// err.Error() is logged for operator diagnosis but the HTTP
		// response stays generic. The reverse-RPC worker's same guard
		// (internal/upstream/connector.go update_config) does surface
		// the detail because the primary is already trusted enough to
		// hold config, but dashboard users may be more broadly authz'd.
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

	writeOK(w)
}

// POST /api/projects/favorite?name=...&favorite=true|false
func (h *ProjectHandlers) handleFavoriteToggle(w http.ResponseWriter, r *http.Request) {
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
		writeJSON(w, map[string]any{"status": "ok", "favorite": favorite})
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
	// Bump the router's version counter so the dashboard's version-based
	// change detection in fetchSessions() notices the favorite flip. Without
	// this, the frontend short-circuits and the star icon only refreshes on
	// the next real session event.
	if h.router != nil {
		h.router.BumpVersion()
	}
	writeJSON(w, map[string]any{"status": "ok", "favorite": favorite})
}

// POST /api/projects/planner/restart?name=...
func (h *ProjectHandlers) handlePlannerRestart(w http.ResponseWriter, r *http.Request) {
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
		writeJSON(w, map[string]string{"status": "restarting"})
		return
	}

	if h.projectMgr == nil {
		http.Error(w, "projects not configured", http.StatusBadRequest)
		return
	}

	// Delegate planner-view opts derivation to Resolver
	// (ResolveForPlannerKey). This preserves the "do not read defaults"
	// contract that keeps administrative planner restarts decoupled from
	// agent defaults (docs/rfc/key-resolver.md §2.2 #6). Legacy fallback
	// reproduces the original literal-AgentOpts construction for headless
	// test paths that don't wire a resolver.
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
		// R215-SEC-P1-2 (#535): mirror the resolver path's spawn-boundary
		// re-validation. EffectivePlannerPrompt re-reads from cached
		// project.yaml / CLAUDE.md, neither of which guarantees the bytes
		// still satisfy ValidateConfig — Claude's Write tool can mutate
		// CLAUDE.md and the next planner restart would inherit the
		// tampered prompt without this sanitiser. Drop the prompt entirely
		// when sanitisation fails so the spawn falls through to "no
		// planner system prompt" rather than feeding control bytes /
		// oversize argv into the CLI subprocess.
		if pp := session.SanitisePlannerPromptForSpawn(h.projectMgr.EffectivePlannerPrompt(p), p.Name); pp != "" {
			opts.ExtraArgs = []string{"--append-system-prompt", pp}
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

	writeJSON(w, map[string]string{"status": "restarted"})
}
