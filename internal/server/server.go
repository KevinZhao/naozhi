package server

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
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
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/transcribe"
	"golang.org/x/time/rate"
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
	msgQueue          *dispatch.MessageQueue
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
	cliH        *CLIBackendsHandler

	// Watchdog kill counters — incremented atomically, exposed via /health and /api/sessions.
	watchdogNoOutputKills atomic.Int64
	watchdogTotalKills    atomic.Int64

	// Watchdog configuration stored for user-facing timeout error messages.
	noOutputTimeout time.Duration
	totalTimeout    time.Duration

	onReady func() // called after listener is bound

	// knownNodes holds all configured node IDs → display names, including
	// reverse nodes that may currently be disconnected. Never mutated after startup.
	knownNodes map[string]string
}

// validateWorkspace checks that workspace is an existing directory within allowedRoot.
// Returns the cleaned, symlink-resolved path or an error.
//
// Ordering: EvalSymlinks is performed first so the root-prefix check sees the
// canonical resolved path; only then do we Stat the resolved form. This
// collapses the TOCTOU window where a symlink swap between an initial Stat
// and a later EvalSymlinks could cause the two calls to observe different
// filesystem entries.
//
// Errors are deliberately generic — the resolved path and underlying
// os.PathError are NOT included so a dashboard or IM user cannot enumerate
// host filesystem layout via crafted workspace queries. Diagnostic detail
// goes to the caller's slog.
func validateWorkspace(workspace, allowedRoot string) (string, error) {
	if workspace == "" {
		return "", fmt.Errorf("workspace is not a valid directory")
	}
	// Explicit absolute-path contract: filepath.Clean preserves relative input
	// verbatim, and when allowedRoot is absolute the HasPrefix check below
	// will always fail for a relative workspace — correct today but implicit.
	// Reject upfront so a future relative allowedRoot cannot silently admit
	// `../etc/passwd` style traversal.
	if !filepath.IsAbs(workspace) {
		return "", fmt.Errorf("workspace is not a valid directory")
	}
	wsPath := filepath.Clean(workspace)
	resolved, err := filepath.EvalSymlinks(wsPath)
	if err != nil {
		// *os.PathError echoes the same path back through err.Error() which
		// lands in debug logs twice. Reduce to a structural kind so operators
		// can still distinguish "not exist" from "permission denied" without
		// a duplicate path column.
		slog.Debug("validateWorkspace: EvalSymlinks failed",
			"path", wsPath, "reason", pathErrReason(err))
		return "", fmt.Errorf("workspace is not a valid directory")
	}
	wsPath = resolved
	info, err := os.Stat(wsPath)
	if err != nil || !info.IsDir() {
		reason := "not_a_directory"
		if err != nil {
			reason = pathErrReason(err)
		}
		slog.Debug("validateWorkspace: Stat failed",
			"path", wsPath, "reason", reason)
		return "", fmt.Errorf("workspace is not a valid directory")
	}
	if allowedRoot != "" && wsPath != allowedRoot &&
		!strings.HasPrefix(wsPath, allowedRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("workspace not allowed")
	}
	return wsPath, nil
}

// pathErrReason returns a short, path-free tag describing a filesystem error
// so debug logs do not echo the workspace path twice via *os.PathError.
func pathErrReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, fs.ErrNotExist):
		return "not_exist"
	case errors.Is(err, fs.ErrPermission):
		return "permission_denied"
	default:
		return "fs_error"
	}
}

// loadOrCreateCookieSecret reads a 32-byte secret from stateDir/cookie_secret,
// creating it with crypto/rand if absent. Falls back to a fresh ephemeral secret
// if the file cannot be read or written (e.g. no stateDir configured).
func loadOrCreateCookieSecret(stateDir string) []byte {
	if stateDir != "" {
		path := filepath.Join(stateDir, "cookie_secret")
		if fi, err := os.Stat(path); err == nil {
			if fi.Mode().Perm() != 0600 {
				// Log at Error with an explicit reason so monitoring can
				// distinguish "unsafe perms forced rotation" from first-run
				// regeneration. All existing browser sessions will be
				// invalidated — operator should know why.
				slog.Error("cookie_secret regenerated due to unsafe permissions",
					"path", path, "mode", fi.Mode().Perm(), "reason", "unsafe_permissions")
			} else if data, err := os.ReadFile(path); err == nil && len(data) == 32 {
				return data
			}
		}
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			panic("crypto/rand unavailable: " + err.Error())
		}
		if err := os.MkdirAll(stateDir, 0700); err == nil {
			// Write atomically (tmp + rename) so a concurrent reader never
			// sees a partial secret during rotation. os.WriteFile opens with
			// O_TRUNC and the crypto/rand bytes land in small chunks — a
			// parallel open+read could pick up N bytes of zeros if we were
			// mid-Write. WriteFileAtomic also fsyncs the file + parent dir.
			if err := osutil.WriteFileAtomic(path, b, 0600); err != nil {
				slog.Warn("cookie_secret atomic write failed; session secret is ephemeral", "err", err)
			}
		}
		return b
	}
	// No stateDir: ephemeral secret (sessions lost on restart)
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
	StateDir          string // directory for persistent state (cookie_secret, etc.)
	NoOutputTimeout   time.Duration
	TotalTimeout      time.Duration
	QueueMaxDepth     int
	QueueCollectDelay time.Duration
	DashboardToken    string // optional bearer token for dashboard API
	TrustedProxy      bool   // trust X-Forwarded-For for client IP
	ProjectManager    *project.Manager
	Nodes             map[string]node.Conn
	ReverseNodeServer *node.ReverseServer
	Transcriber       transcribe.Service
	OnReady           func() // called after the listener is bound and serving
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

	// allowed_root is the one directory-traversal guard for dashboard /cd,
	// cron WorkDir, and handleTakeover CWD. Empty means "no restriction",
	// which is the legitimate single-user default but a real risk in
	// multi-user deployments. Surface it once at startup so operators can
	// audit their config rather than discovering the looseness via incident.
	if opts.AllowedRoot == "" {
		slog.Warn("server.allowed_root is unset; dashboard /cd, cron WorkDir, and takeover CWD accept any absolute path — set allowed_root in config.yaml to restrict")
	}

	cookieSecret := loadOrCreateCookieSecret(opts.StateDir)

	s := &Server{
		addr:            addr,
		mux:             http.NewServeMux(),
		platforms:       platforms,
		router:          router,
		dedup:           platform.NewDedup(defaultDedupCapacity),
		sessionGuard:    session.NewGuard(),
		msgQueue:        dispatch.NewMessageQueue(opts.QueueMaxDepth, opts.QueueCollectDelay),
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
		onReady:         opts.OnReady,
		projectMgr:      opts.ProjectManager,
		nodes:           nodes,
		knownNodes:      knownNodes,

		// Extracted handler groups
		auth: &AuthHandlers{
			dashboardToken: opts.DashboardToken,
			cookieSecret:   cookieSecret,
			loginLimiter:   newLoginLimiter(),
			trustedProxy:   opts.TrustedProxy,
		},
		cronH: &CronHandlers{
			scheduler:   scheduler,
			allowedRoot: opts.AllowedRoot,
		},
		transcribeH: &TranscribeHandler{
			transcriber:       opts.Transcriber,
			transcribeLimiter: newIPLimiterWithProxy(rate.Every(12*time.Second), 5, opts.TrustedProxy), // 5 transcriptions/min per IP
			sem:               make(chan struct{}, transcribeSemCap),
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
	agentIDs := make([]string, 0, len(agents)+1)
	agentIDs = append(agentIDs, "general")
	for id := range agents {
		agentIDs = append(agentIDs, id)
	}
	s.sessionH = &SessionHandlers{
		router:        router,
		projectMgr:    opts.ProjectManager,
		claudeDir:     claudeDir,
		allowedRoot:   opts.AllowedRoot,
		agents:        agents,
		agentIDs:      agentIDs,
		nodeAccess:    s.nodeAccess,
		nodeCache:     s.nodeCache,
		startedAt:     s.startedAt,
		backendTag:    tag,
		workspaceID:   opts.WorkspaceID,
		workspaceName: opts.WorkspaceName,
		watchdogNoOut: &s.watchdogNoOutputKills,
		watchdogTotal: &s.watchdogTotalKills,
	}
	s.sessionH.WarmHistoryCache()
	s.cliH = NewCLIBackendsHandler(router)
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
	d := dispatch.NewDispatcher(dispatch.DispatcherConfig{
		Router:                s.router,
		Platforms:             s.platforms,
		Agents:                s.agents,
		AgentCommands:         s.agentCommands,
		Scheduler:             s.scheduler,
		ProjectMgr:            s.projectMgr,
		Guard:                 s.sessionGuard,
		Queue:                 s.msgQueue,
		Dedup:                 s.dedup,
		AllowedRoot:           s.allowedRoot,
		ClaudeDir:             s.claudeDir,
		ReplyFooter:           s.backendTag,
		SendFn:                s.sendWithBroadcast,
		TakeoverFn:            s.tryAutoTakeover,
		NoOutputTimeout:       s.noOutputTimeout,
		TotalTimeout:          s.totalTimeout,
		WatchdogNoOutputKills: &s.watchdogNoOutputKills,
		WatchdogTotalKills:    &s.watchdogTotalKills,
	})
	// Expose dispatcher counters via /health. The handler is constructed
	// earlier in New() without a dispatcher reference, so we wire the
	// closure here once the dispatcher exists.
	if s.healthH != nil {
		s.healthH.dispatcherMetrics = d.Metrics
	}
	handler := d.BuildHandler()

	var startedPlatforms []platform.RunnablePlatform
	for _, p := range s.platforms {
		p.RegisterRoutes(s.mux, handler)
		slog.Info("platform registered", "name", p.Name())

		if rp, ok := p.(platform.RunnablePlatform); ok {
			if err := rp.Start(handler); err != nil {
				// Stop already-started platforms to avoid connection leaks.
				// Log individual stop failures; a silent rollback could mask
				// a dangling websocket that holds the process open past the
				// fatal startup error we're about to return.
				for _, sp := range startedPlatforms {
					if stopErr := sp.Stop(); stopErr != nil {
						slog.Warn("platform rollback stop failed",
							"name", sp.Name(), "err", stopErr)
					}
				}
				return fmt.Errorf("start platform %s: %w", p.Name(), err)
			}
			startedPlatforms = append(startedPlatforms, rp)
		}
	}

	s.mux.HandleFunc("GET /health", s.healthH.handleHealth)
	s.discoveryH.appCtx = ctx
	s.registerDashboard()
	s.nodeCache.StartLoop(ctx)
	s.discoveryCache.startLoop(ctx)
	s.startProjectScanLoop(ctx)
	// Warn if we're serving a token-protected dashboard over plaintext with no
	// trusted proxy in front — Bearer tokens and auth cookies would traverse
	// the wire in the clear, subject to passive sniffing on shared networks.
	// `trustedProxy=true` is the operator's explicit statement that TLS
	// termination happens upstream (ALB/CloudFront), in which case this
	// listener binding to plaintext loopback is fine.
	if s.dashboardToken != "" && !s.auth.trustedProxy && isPlaintextPublicAddr(s.addr) {
		slog.Warn(
			"dashboard token served over plaintext HTTP with no trusted proxy: "+
				"bearer tokens and session cookies may be sniffed. "+
				"Terminate TLS upstream and set server.trusted_proxy=true, "+
				"or bind to 127.0.0.1 for local-only access.",
			"addr", s.addr,
		)
	}
	slog.Info("server starting", "addr", s.addr)

	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.addr, err)
	}

	srv := &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second, // Slowloris defense
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Cap header bytes well below the default 1 MB so an unauthenticated
		// client can't force us to buffer megabyte-sized headers before
		// ReadHeaderTimeout fires. 64 KB is generous for legitimate cookies
		// plus a modest number of X-Forwarded-* headers.
		MaxHeaderBytes: 64 * 1024,
	}

	// Notify caller that the listener is bound and ready to accept connections.
	if s.onReady != nil {
		s.onReady()
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

	err = srv.Serve(ln)
	// If ListenAndServe failed for a non-shutdown reason (e.g. port conflict),
	// return immediately instead of blocking — the shutdown goroutine is still
	// waiting on ctx.Done and shutdownComplete will never close.
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// Wait for the shutdown goroutine to finish draining connections.
	select {
	case <-shutdownComplete:
	case <-ctx.Done():
		<-shutdownComplete
	}
	return err
}

// isPlaintextPublicAddr reports whether addr is a non-loopback TCP listen
// address that would expose Bearer tokens and auth cookies over cleartext
// HTTP. Loopback (127.0.0.1 / ::1 / localhost) is considered safe because
// the traffic never leaves the host. Addresses we cannot parse are treated
// as public so the warning errs on the side of visibility.
func isPlaintextPublicAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// ":8080" form — no host, bound to all interfaces, public by default.
		return true
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return true
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return false
	}
	return true
}

// startProjectScanLoop periodically rescans the projects root for CLAUDE.md changes
// and cleans up orphaned planner sessions for removed projects.
func (s *Server) startProjectScanLoop(ctx context.Context) {
	if s.projectMgr == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(session.ProjectScanInterval)
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
