package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

func (s *Server) handleAPIProjects(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearerAuth(w, r) {
		return
	}
	if s.projectMgr == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]any{})
		return
	}

	projects := s.projectMgr.All()
	result := make([]map[string]any, 0, len(projects))
	for _, p := range projects {
		plannerKey := p.PlannerSessionKey()
		plannerState := "none"
		if sess := s.router.GetSession(plannerKey); sess != nil {
			snap := sess.Snapshot()
			plannerState = snap.State
		}

		result = append(result, map[string]any{
			"name":          p.Name,
			"path":          p.Path,
			"planner_state": plannerState,
			"planner_model": s.projectMgr.EffectivePlannerModel(p),
			"config":        p.Config,
		})
	}

	// Merge remote projects
	s.nodesMu.RLock()
	hasNodes := len(s.nodes) > 0
	s.nodesMu.RUnlock()
	if hasNodes {
		allProjects := make([]any, 0, len(result))
		for _, r := range result {
			r["node"] = "local"
			allProjects = append(allProjects, r)
		}
		cachedProjects := s.nodeCache.Projects()
		for _, items := range cachedProjects {
			for _, item := range items {
				allProjects = append(allProjects, item)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(allProjects); err != nil {
			slog.Error("encode projects response", "err", err)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		slog.Error("encode projects response", "err", err)
	}
}

func (s *Server) handleAPIProjectConfigGet(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearerAuth(w, r) {
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" || s.projectMgr == nil {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	p := s.projectMgr.Get(name)
	if p == nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(p.Config); err != nil {
		slog.Error("encode project config", "err", err)
	}
}

func (s *Server) handleAPIProjectConfigPut(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearerAuth(w, r) {
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	// Remote node proxy
	node := r.URL.Query().Get("node")
	if node != "" && node != "local" {
		s.nodesMu.RLock()
		nc, ok := s.nodes[node]
		s.nodesMu.RUnlock()
		if !ok {
			http.Error(w, "unknown node", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body failed", http.StatusBadRequest)
			return
		}
		if err := nc.ProxyUpdateConfig(r.Context(), name, body); err != nil {
			slog.Warn("proxy update config failed", "node", node, "err", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		return
	}

	if s.projectMgr == nil {
		http.Error(w, "projects not configured", http.StatusBadRequest)
		return
	}

	var cfg project.ProjectConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if err := s.projectMgr.UpdateConfig(name, cfg); err != nil {
		if errors.Is(err, project.ErrNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleAPIProjectPlannerRestart(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearerAuth(w, r) {
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	// Remote node proxy
	node := r.URL.Query().Get("node")
	if node != "" && node != "local" {
		s.nodesMu.RLock()
		nc, ok := s.nodes[node]
		s.nodesMu.RUnlock()
		if !ok {
			http.Error(w, "unknown node", http.StatusBadRequest)
			return
		}
		if err := nc.ProxyRestartPlanner(r.Context(), name); err != nil {
			slog.Warn("proxy restart planner failed", "node", node, "err", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "restarting"})
		return
	}

	if s.projectMgr == nil {
		http.Error(w, "projects not configured", http.StatusBadRequest)
		return
	}

	p := s.projectMgr.Get(name)
	if p == nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}

	// Reset and spawn a new planner atomically with current config
	plannerKey := p.PlannerSessionKey()
	opts := session.AgentOpts{
		Model:     s.projectMgr.EffectivePlannerModel(p),
		Workspace: p.Path,
		Exempt:    true,
	}
	if prompt := s.projectMgr.EffectivePlannerPrompt(p); prompt != "" {
		opts.ExtraArgs = []string{"--append-system-prompt", prompt}
	}

	go func() {
		ctx := context.Background()
		if s.hub != nil {
			ctx = s.hub.ctx
		}
		if _, err := s.router.ResetAndRecreate(ctx, plannerKey, opts); err != nil {
			slog.Error("planner restart failed", "project", name, "err", err)
		} else {
			slog.Info("planner restarted", "project", name)
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "restarting"})
}
