package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/transcribe"
)

const (
	defaultDedupCapacity = 10000
	shutdownTimeout      = 30 * time.Second
)

// Server is the HTTP entry point for Naozhi.
type Server struct {
	addr              string
	mux               *http.ServeMux
	platforms         map[string]platform.Platform
	router            *session.Router
	dedup             *platform.Dedup
	sessionGuard      *sessionGuard
	startedAt         time.Time
	agents            map[string]session.AgentOpts
	agentCommands     map[string]string
	scheduler         *cron.Scheduler
	backendTag        string // e.g., "cc" or "kiro", appended to replies
	dashboardToken    string // optional bearer token for dashboard API
	hub               *Hub   // WebSocket hub
	nodes             map[string]NodeConn
	reverseNodeServer *ReverseNodeServer
	nodesMu           sync.RWMutex
	claudeDir         string // path to ~/.claude for session discovery
	projectMgr        *project.Manager
	workspaceID       string // local workspace identity
	workspaceName     string
	allowedRoot       string // /cd is restricted to paths under this directory
	transcriber       transcribe.Service
	nodeCache         *NodeCacheManager // background-cached remote node data
	discoveryCache    *discoveryCache   // background-cached local discovery results

	// Watchdog configuration stored for user-facing timeout error messages.
	noOutputTimeout time.Duration
	totalTimeout    time.Duration

	// Watchdog kill counters — incremented atomically, exposed via /health.
	watchdogNoOutputKills atomic.Int64
	watchdogTotalKills    atomic.Int64

	// knownNodes holds all configured node IDs → display names, including
	// reverse nodes that may currently be disconnected. Never mutated after startup.
	knownNodes map[string]string
}

// sessionGuard prevents multiple concurrent messages to the same session.
type sessionGuard struct {
	active   sync.Map             // string → struct{}: sessions currently processing a message
	waitMu   sync.Mutex           // guards lastWait
	lastWait map[string]time.Time // tracks last "please wait" reply per key
}

func newSessionGuard() *sessionGuard {
	return &sessionGuard{
		lastWait: make(map[string]time.Time),
	}
}

func (g *sessionGuard) TryAcquire(key string) bool {
	_, loaded := g.active.LoadOrStore(key, struct{}{})
	return !loaded
}

// ShouldSendWait returns true if enough time has passed since the last
// "please wait" reply for this key (avoids spamming the user).
func (g *sessionGuard) ShouldSendWait(key string) bool {
	g.waitMu.Lock()
	defer g.waitMu.Unlock()
	if time.Since(g.lastWait[key]) < 3*time.Second {
		return false
	}
	g.lastWait[key] = time.Now()
	return true
}

func (g *sessionGuard) Release(key string) {
	g.active.Delete(key)
	g.waitMu.Lock()
	delete(g.lastWait, key)
	g.waitMu.Unlock()
}

// New creates a new Server.
// ServerOptions holds optional configuration for a Server.
// All fields have zero-value defaults (empty string, nil, zero duration = disabled/unset).
type ServerOptions struct {
	WorkspaceID       string
	WorkspaceName     string
	AllowedRoot       string // restricts /cd to paths under this root
	NoOutputTimeout   time.Duration
	TotalTimeout      time.Duration
	DashboardToken    string // optional bearer token for dashboard API
	ProjectManager    *project.Manager
	Nodes             map[string]NodeConn
	ReverseNodeServer *ReverseNodeServer
	Transcriber       transcribe.Service
}

func New(addr string, router *session.Router, platforms map[string]platform.Platform, agents map[string]session.AgentOpts, agentCommands map[string]string, scheduler *cron.Scheduler, backend string, opts ServerOptions) *Server {
	tag := "cc"
	if backend == "kiro" {
		tag = "kiro"
	}
	claudeDir := ""
	if home, err := os.UserHomeDir(); err == nil {
		claudeDir = filepath.Join(home, ".claude")
	}

	nodes := opts.Nodes
	if nodes == nil {
		nodes = make(map[string]NodeConn)
	}
	knownNodes := make(map[string]string)
	for id, nc := range nodes {
		knownNodes[id] = nc.DisplayName()
	}

	s := &Server{
		addr:            addr,
		mux:             http.NewServeMux(),
		platforms:       platforms,
		router:          router,
		dedup:           platform.NewDedup(defaultDedupCapacity),
		sessionGuard:    newSessionGuard(),
		startedAt:       time.Now(),
		agents:          agents,
		agentCommands:   agentCommands,
		scheduler:       scheduler,
		backendTag:      tag,
		claudeDir:       claudeDir,
		workspaceID:     opts.WorkspaceID,
		workspaceName:   opts.WorkspaceName,
		allowedRoot:     opts.AllowedRoot,
		noOutputTimeout: opts.NoOutputTimeout,
		totalTimeout:    opts.TotalTimeout,
		dashboardToken:  opts.DashboardToken,
		projectMgr:      opts.ProjectManager,
		transcriber:     opts.Transcriber,
		nodes:           nodes,
		knownNodes:      knownNodes,
	}

	s.nodeCache = NewNodeCacheManager(
		func() map[string]NodeConn {
			s.nodesMu.RLock()
			defer s.nodesMu.RUnlock()
			cp := make(map[string]NodeConn, len(s.nodes))
			for k, v := range s.nodes {
				cp[k] = v
			}
			return cp
		},
		func() {
			if s.hub != nil {
				s.hub.BroadcastSessionsUpdate()
			}
		},
	)

	s.discoveryCache = newDiscoveryCache(claudeDir, s.router.ManagedExcludeSets, opts.ProjectManager)

	if opts.ReverseNodeServer != nil {
		s.reverseNodeServer = opts.ReverseNodeServer
		for id, displayName := range opts.ReverseNodeServer.AllNodes() {
			s.knownNodes[id] = displayName
		}
		opts.ReverseNodeServer.onRegister = func(id string, rc *ReverseNodeConn) {
			s.nodesMu.Lock()
			s.nodes[id] = rc
			s.nodesMu.Unlock()
			go s.nodeCache.RefreshFor(id) // RefreshFor calls onChange → BroadcastSessionsUpdate
		}
		opts.ReverseNodeServer.onDeregister = func(id string) {
			s.nodesMu.Lock()
			delete(s.nodes, id)
			s.nodesMu.Unlock()
			s.nodeCache.PurgeNode(id)
			if s.hub != nil {
				s.hub.PurgeNodeSubscriptions(id)
				s.hub.BroadcastSessionsUpdate()
			}
		}
	}

	return s
}

// Start registers routes and begins serving.
func (s *Server) Start(ctx context.Context) error {
	handler := s.buildMessageHandler()

	var startedPlatforms []platform.RunnablePlatform
	for _, p := range s.platforms {
		p.RegisterRoutes(s.mux, handler)
		slog.Info("platform registered", "name", p.Name())

		if rp, ok := p.(platform.RunnablePlatform); ok {
			if err := rp.Start(handler); err != nil {
				// Stop already-started platforms to avoid connection leaks
				for _, sp := range startedPlatforms {
					sp.Stop()
				}
				return fmt.Errorf("start platform %s: %w", p.Name(), err)
			}
			startedPlatforms = append(startedPlatforms, rp)
		}
	}

	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.registerDashboard()
	s.nodeCache.StartLoop(ctx)
	s.discoveryCache.startLoop(ctx)
	s.startProjectScanLoop(ctx)
	slog.Info("server starting", "addr", s.addr)

	srv := &http.Server{
		Addr:         s.addr,
		Handler:      s.mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		slog.Info("shutting down server")

		// Shutdown WebSocket hub
		if s.hub != nil {
			s.hub.Shutdown()
		}

		// Stop RunnablePlatforms (e.g. WebSocket connections)
		for _, p := range s.platforms {
			if rp, ok := p.(platform.RunnablePlatform); ok {
				if err := rp.Stop(); err != nil {
					slog.Error("stop platform", "name", p.Name(), "err", err)
				}
			}
		}

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("server shutdown error", "err", err)
		}
	}()

	return srv.ListenAndServe()
}

// startProjectScanLoop periodically rescans the projects root for CLAUDE.md changes
// and cleans up orphaned planner sessions for removed projects.
func (s *Server) startProjectScanLoop(ctx context.Context) {
	if s.projectMgr == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				oldNames := s.projectMgr.ProjectNames()
				if err := s.projectMgr.Scan(); err != nil {
					slog.Warn("project rescan", "err", err)
					continue
				}
				newNames := s.projectMgr.ProjectNames()

				// Detect removed projects and clean up orphaned planner sessions
				changed := len(oldNames) != len(newNames)
				for name := range oldNames {
					if _, ok := newNames[name]; !ok {
						changed = true
						plannerKey := project.PlannerKeyFor(name)
						if s.router.Remove(plannerKey) {
							slog.Info("removed orphaned planner", "project", name)
						}
					}
				}
				if changed {
					slog.Info("project list changed", "count", len(newNames))
					if s.hub != nil {
						s.hub.BroadcastSessionsUpdate()
					}
				}
			}
		}
	}()
}
