package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// plannerModelRe restricts the model identifier to safe characters so a
// crafted value cannot sneak extra CLI flags (e.g. " --dangerously-skip-permissions")
// into the exec.Command argv for the planner CLI. Whitespace, dashes at the
// start, and control characters are rejected.
var plannerModelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\-]*$`)

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
			"git_remote_url": p.GitRemoteURL,
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
	if name == "" || h.projectMgr == nil {
		http.Error(w, "name is required", http.StatusBadRequest)
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
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
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
		writeJSON(w, map[string]string{"status": "ok"})
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

	// Bound free-form fields that end up as args to exec.Command. An
	// oversized PlannerPrompt would inflate the command line past ARG_MAX
	// (Linux ~2 MB) and make Spawn fail with a cryptic E2BIG.
	const (
		maxPlannerPromptBytes = 8 * 1024
		maxPlannerModelBytes  = 256
	)
	if len(cfg.PlannerPrompt) > maxPlannerPromptBytes {
		http.Error(w, fmt.Sprintf("planner_prompt exceeds %d-byte limit", maxPlannerPromptBytes), http.StatusBadRequest)
		return
	}
	if len(cfg.PlannerModel) > maxPlannerModelBytes {
		http.Error(w, fmt.Sprintf("planner_model exceeds %d-byte limit", maxPlannerModelBytes), http.StatusBadRequest)
		return
	}
	// Reject any model value that isn't a plain identifier. Without this guard
	// a value like "foo --dangerously-skip-permissions" would be appended to
	// exec.Command's argv for the planner CLI — exec doesn't parse flags but
	// the child CLI does, and a user-controllable flag injection extends the
	// dashboard's blast radius.
	if cfg.PlannerModel != "" && !plannerModelRe.MatchString(cfg.PlannerModel) {
		http.Error(w, "planner_model contains invalid characters", http.StatusBadRequest)
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

	writeJSON(w, map[string]string{"status": "ok"})
}

// POST /api/projects/favorite?name=...&favorite=true|false
func (h *ProjectHandlers) handleFavoriteToggle(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
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
	writeJSON(w, map[string]any{"status": "ok", "favorite": favorite})
}

// POST /api/projects/planner/restart?name=...
func (h *ProjectHandlers) handlePlannerRestart(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
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
