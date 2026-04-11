package server

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/transcribe"
)

const defaultDedupCapacity = 10000

// Server is the HTTP entry point for Naozhi.
type Server struct {
	addr              string
	mux               *http.ServeMux
	platforms         map[string]platform.Platform
	router            *session.Router
	dedup             *platform.Dedup
	sessionGuard      *session.Guard
	startedAt         time.Time
	agents            map[string]session.AgentOpts
	agentCommands     map[string]string
	scheduler         *cron.Scheduler
	backendTag        string // e.g., "cc" or "kiro", appended to replies
	dashboardToken    string // optional bearer token for dashboard API
	hub               *Hub   // WebSocket hub
	nodes             map[string]node.Conn
	reverseNodeServer *node.ReverseServer
	nodesMu           sync.RWMutex
	claudeDir         string // path to ~/.claude for session discovery
	projectMgr        *project.Manager
	workspaceName     string
	allowedRoot       string             // /cd is restricted to paths under this directory (used by Hub)
	nodeCache         *node.CacheManager // background-cached remote node data
	discoveryCache    *discoveryCache    // background-cached local discovery results

	// Extracted handler groups
	auth        *AuthHandlers
	cronH       *CronHandlers
	transcribeH *TranscribeHandler
	nodeAccess  *nodeAccessor
	discoveryH  *DiscoveryHandlers
	projectH    *ProjectHandlers
	sessionH    *SessionHandlers
	healthH     *HealthHandler
	sendH       *SendHandler

	// Watchdog kill counters — incremented atomically, exposed via /health and /api/sessions.
	watchdogNoOutputKills atomic.Int64
	watchdogTotalKills    atomic.Int64

	// Watchdog configuration stored for user-facing timeout error messages.
	noOutputTimeout time.Duration
	totalTimeout    time.Duration

	// knownNodes holds all configured node IDs → display names, including
	// reverse nodes that may currently be disconnected. Never mutated after startup.
	knownNodes map[string]string
}

// validateWorkspace checks that workspace is an existing directory within allowedRoot.
// Returns the cleaned, symlink-resolved path or an error.
func validateWorkspace(workspace, allowedRoot string) (string, error) {
	info, err := os.Stat(workspace)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("workspace is not a valid directory")
	}
	wsPath := filepath.Clean(workspace)
	resolved, err := filepath.EvalSymlinks(wsPath)
	if err != nil {
		return "", fmt.Errorf("resolve workspace path: %w", err)
	}
	wsPath = resolved
	if allowedRoot != "" && wsPath != allowedRoot &&
		!strings.HasPrefix(wsPath, allowedRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("workspace outside allowed root")
	}
	return wsPath, nil
}

// generateCookieSecret returns 32 random bytes for HMAC-signing auth cookies.
func generateCookieSecret() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return b
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
	Nodes             map[string]node.Conn
	ReverseNodeServer *node.ReverseServer
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
		nodes = make(map[string]node.Conn)
	}
	knownNodes := make(map[string]string)
	for id, nc := range nodes {
		knownNodes[id] = nc.DisplayName()
	}

	cookieSecret := generateCookieSecret()

	s := &Server{
		addr:            addr,
		mux:             http.NewServeMux(),
		platforms:       platforms,
		router:          router,
		dedup:           platform.NewDedup(defaultDedupCapacity),
		sessionGuard:    session.NewGuard(),
		startedAt:       time.Now(),
		agents:          agents,
		agentCommands:   agentCommands,
		scheduler:       scheduler,
		backendTag:      tag,
		claudeDir:       claudeDir,
		workspaceName:   opts.WorkspaceName,
		allowedRoot:     opts.AllowedRoot,
		noOutputTimeout: opts.NoOutputTimeout,
		totalTimeout:    opts.TotalTimeout,
		dashboardToken:  opts.DashboardToken,
		projectMgr:      opts.ProjectManager,
		nodes:           nodes,
		knownNodes:      knownNodes,

		// Extracted handler groups
		auth: &AuthHandlers{
			dashboardToken: opts.DashboardToken,
			cookieSecret:   cookieSecret,
			loginLimiters:  loginLimiterStore{entries: make(map[string]*limiterEntry)},
		},
		cronH: &CronHandlers{
			scheduler:   scheduler,
			allowedRoot: opts.AllowedRoot,
		},
		transcribeH: &TranscribeHandler{
			transcriber: opts.Transcriber,
		},
	}

	s.nodeAccess = newNodeAccessor(&s.nodesMu, s.nodes, s.knownNodes)

	s.nodeCache = node.NewCacheManager(
		func() map[string]node.Conn {
			return s.nodeAccess.NodesSnapshot()
		},
		func() {
			if s.hub != nil {
				s.hub.BroadcastSessionsUpdate()
			}
		},
	)

	s.discoveryCache = newDiscoveryCache(claudeDir, s.router.ManagedExcludeSets, opts.ProjectManager)

	// Wire extracted handler groups that depend on nodeAccess/nodeCache
	s.discoveryH = &DiscoveryHandlers{
		discoveryCache: s.discoveryCache,
		nodeAccess:     s.nodeAccess,
		nodeCache:      s.nodeCache,
		claudeDir:      claudeDir,
		router:         router,
		allowedRoot:    opts.AllowedRoot,
		defaultAgent:   agents["general"],
		broadcast: func() {
			if s.hub != nil {
				s.hub.BroadcastSessionsUpdate()
			}
		},
	}
	s.projectH = &ProjectHandlers{
		projectMgr: opts.ProjectManager,
		router:     router,
		nodeAccess: s.nodeAccess,
		nodeCache:  s.nodeCache,
		ctxFunc: func() context.Context {
			if s.hub != nil {
				return s.hub.ctx
			}
			return context.Background()
		},
	}
	s.sessionH = &SessionHandlers{
		router:        router,
		projectMgr:    opts.ProjectManager,
		claudeDir:     claudeDir,
		allowedRoot:   opts.AllowedRoot,
		agents:        agents,
		nodeAccess:    s.nodeAccess,
		nodeCache:     s.nodeCache,
		startedAt:     s.startedAt,
		backendTag:    tag,
		workspaceID:   opts.WorkspaceID,
		workspaceName: opts.WorkspaceName,
		watchdogNoOut: &s.watchdogNoOutputKills,
		watchdogTotal: &s.watchdogTotalKills,
	}
	platNames := make(map[string]struct{}, len(platforms))
	for name := range platforms {
		platNames[name] = struct{}{}
	}
	s.healthH = &HealthHandler{
		router:          router,
		auth:            s.auth,
		startedAt:       s.startedAt,
		workspaceID:     opts.WorkspaceID,
		workspaceName:   opts.WorkspaceName,
		noOutputTimeout: opts.NoOutputTimeout,
		totalTimeout:    opts.TotalTimeout,
		watchdogNoOut:   &s.watchdogNoOutputKills,
		watchdogTotal:   &s.watchdogTotalKills,
		nodeAccess:      s.nodeAccess,
		platforms:       platNames,
		hubDropped: func() int64 {
			if s.hub == nil {
				return 0
			}
			return s.hub.DroppedMessages()
		},
	}
	// sendH is wired after registerDashboard creates hub

	if opts.ReverseNodeServer != nil {
		s.reverseNodeServer = opts.ReverseNodeServer
		for id, displayName := range opts.ReverseNodeServer.AllNodes() {
			s.knownNodes[id] = displayName
		}
		opts.ReverseNodeServer.OnRegister = func(id string, rc *node.ReverseConn) {
			s.nodesMu.Lock()
			s.nodes[id] = rc
			s.nodesMu.Unlock()
			go s.nodeCache.RefreshFor(id) // RefreshFor calls onChange → BroadcastSessionsUpdate
		}
		opts.ReverseNodeServer.OnDeregister = func(id string) {
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
	d := &dispatch.Dispatcher{
		Router:                s.router,
		Platforms:             s.platforms,
		Agents:                s.agents,
		AgentCommands:         s.agentCommands,
		Scheduler:             s.scheduler,
		ProjectMgr:            s.projectMgr,
		Guard:                 s.sessionGuard,
		Dedup:                 s.dedup,
		AllowedRoot:           s.allowedRoot,
		ClaudeDir:             s.claudeDir,
		BackendTag:            s.backendTag,
		SendFn:                s.sendWithBroadcast,
		TakeoverFn:            s.tryAutoTakeover,
		NoOutputTimeout:       s.noOutputTimeout,
		TotalTimeout:          s.totalTimeout,
		WatchdogNoOutputKills: &s.watchdogNoOutputKills,
		WatchdogTotalKills:    &s.watchdogTotalKills,
	}
	handler := d.BuildHandler()

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

	s.mux.HandleFunc("GET /health", s.healthH.handleHealth)
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
	shutdownComplete := make(chan struct{})
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

		shutdownCtx, cancel := context.WithTimeout(context.Background(), session.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("server shutdown error", "err", err)
		}
		close(shutdownComplete)
	}()

	err := srv.ListenAndServe()
	// Wait for the shutdown goroutine to finish draining connections.
	// Only block if shutdown was initiated (ctx cancelled); if ListenAndServe
	// failed for another reason (e.g. port conflict), the goroutine is still
	// waiting on ctx.Done and shutdownComplete will never close.
	select {
	case <-shutdownComplete:
	case <-ctx.Done():
		<-shutdownComplete
	}
	return err
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
