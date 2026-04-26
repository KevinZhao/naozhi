package server

import (
	"context"
	"encoding/json"
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
// Project names are directory names under projects_root; 128 matches the
// session-key component cap and is well above any realistic directory.
const maxProjectNameLen = 128

// validateProjectName rejects oversized or control-character names before
// they flow into slog attrs. Without this a post-auth caller could emit a
// 30 KB ANSI-escape-laden name attr on every log line, bloating log
// storage and (in terminal-rendered log viewers) corrupting output.
// R68-SEC-M3.
func validateProjectName(name string) error {
	if name == "" {
		return errors.New("name is required")
	}
	if len(name) > maxProjectNameLen {
		return errors.New("name too long")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 0x20 || c == 0x7f {
			return errors.New("name contains invalid characters")
		}
	}
	return nil
}

// ProjectHandlers groups the project management API endpoints.
type ProjectHandlers struct {
	projectMgr *project.Manager
	router     *session.Router
	nodeAccess NodeAccessor
	nodeCache  *node.CacheManager
	ctxFunc    func() context.Context // returns hub.ctx or Background
}

// GET /api/projects — list all projects (local + remote).
func (h *ProjectHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	if h.projectMgr == nil {
		writeJSON(w, []any{})
		return
	}

	projects := h.projectMgr.All()
	result := make([]map[string]any, 0, len(projects))
	for _, p := range projects {
		plannerKey := p.PlannerSessionKey()
		plannerState := "none"
		if sess := h.router.GetSession(plannerKey); sess != nil {
			snap := sess.Snapshot()
			plannerState = snap.State
		}

		result = append(result, map[string]any{
			"name":           p.Name,
			"path":           p.Path,
			"planner_state":  plannerState,
			"planner_model":  h.projectMgr.EffectivePlannerModel(p),
			"config":         p.Config,
			"favorite":       p.Config.Favorite,
			"git_remote_url": redactGitRemoteURL(p.GitRemoteURL),
			"github":         p.IsGitHub,
		})
	}

	// Merge remote projects
	if h.nodeAccess.HasNodes() {
		allProjects := make([]any, 0, len(result))
		for _, r := range result {
			r["node"] = "local"
			allProjects = append(allProjects, r)
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
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)

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
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		// Fixed error string: echoing err.Error() leaks the decoder's field
		// names / offsets which help schema enumeration.
		slog.Debug("project config: decode failed", "err", err, "project", name)
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if err := project.ValidateConfig(cfg); err != nil {
		// Surface the wrapped reason directly — it reports *which* field
		// was rejected without leaking decoder internals. The same
		// validator gates the reverse-RPC update_config path in
		// internal/upstream/connector.go. R68-SEC-H2.
		http.Error(w, err.Error(), http.StatusBadRequest)
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

	p := h.projectMgr.Get(name)
	if p == nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}

	// Reset and spawn a new planner atomically with current config
	plannerKey := p.PlannerSessionKey()
	opts := session.AgentOpts{
		Model:     h.projectMgr.EffectivePlannerModel(p),
		Workspace: p.Path,
		Exempt:    true,
	}
	if prompt := h.projectMgr.EffectivePlannerPrompt(p); prompt != "" {
		opts.ExtraArgs = []string{"--append-system-prompt", prompt}
	}

	ctx, cancel := context.WithTimeout(h.ctxFunc(), 30*time.Second)
	defer cancel()
	if _, err := h.router.ResetAndRecreate(ctx, plannerKey, opts); err != nil {
		slog.Error("planner restart failed", "project", name, "err", err)
		http.Error(w, "restart failed", http.StatusInternalServerError)
		return
	}
	slog.Info("planner restarted", "project", name)

	writeJSON(w, map[string]string{"status": "restarted"})
}
