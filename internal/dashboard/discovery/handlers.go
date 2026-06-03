// Package discovery hosts the dashboard endpoints that surface external
// Claude CLI sessions running on this host (and on connected nodes) and
// allow operators to take them over or close them under naozhi management.
//
// Phase 3b (server-split-phase4-design.md §6.5 Plan B): handlers moved
// here from internal/server/dashboard_discovered.go. The DiscoveryHandlers
// type was already a self-contained struct in master with a small Deps
// surface — this PR mostly relocates the file and inverts a couple of
// server-package private dependencies into injected closures so the
// sub-package compiles standalone:
//
//   - ValidateWorkspace / VerifyProcIdentity inject as func parameters
//     (DI; small interface surface — see Handlers struct below)
//   - DiscoveryCacheView interface replaces the *server.discoveryCache
//     direct field (snapshot + evictPID are the only methods used)
//   - NodeAccessor interface is duplicated locally (subset of server.NodeAccessor)
//     so we don't reverse-import internal/server
//
// The 4 handlers (handleList / handlePreview / handleTakeover / handleClose)
// continue to satisfy the existing route table in
// internal/server/dashboard.go::registerDashboard via *DiscoveryHandlers.
package discovery

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"
)

// CacheView is the subset of *server.discoveryCache that this package needs.
// Defining it as a small interface here (per "accept interfaces" idiom) keeps
// the dependency direction one-way: server implements, discovery consumes.
type CacheView interface {
	Snapshot() []discovery.DiscoveredSession
	EvictPID(pid int)
}

// NodeAccessor is the same subset of server.NodeAccessor used by
// DiscoveryHandlers — duplicated locally so we don't reverse-import server.
// The two interfaces stay structurally identical; nodeAccessor in server
// satisfies both.
type NodeAccessor interface {
	HasNodes() bool
	LookupNode(w http.ResponseWriter, id string) (node.Conn, bool)
}

// SessionRouter is the subset of *session.Router this package calls.
// Defined as an interface so future test doubles do not need a full Router.
// Takeover's first return is dropped (*session.ManagedSession is unused by
// these handlers — they only care about success/failure) so we use a
// distinct method name + an adapter at the wiring site, avoiding a
// circular dependency on internal/session for the named return type.
type SessionRouter interface {
	Takeover(ctx context.Context, key, sessionID, cwd string, opts session.AgentOpts) error
}

// Handlers groups the discovered-session and takeover API endpoints.
//
// Lifecycle:
//   - constructed once via New() in internal/server/build_handlers.go
//     before the server context is available
//   - SetAppContext(ctx) is called from server.go after registerDashboard
//     so background takeover/close goroutines can outlive the request
//     ctx but be cancelled at process shutdown
type Handlers struct {
	appCtx          context.Context // server lifecycle context, set via SetAppContext
	bg              sync.WaitGroup  // tracks background takeover/close goroutines for graceful drain
	cache           CacheView
	nodeAccess      NodeAccessor
	nodeCache       *node.CacheManager
	claudeDir       string
	router          SessionRouter
	allowedRoot     string
	defaultAgent    session.AgentOpts // agents["general"]
	broadcast       func()            // hub.BroadcastSessionsUpdate
	validateWS      func(ws, root string) (string, error)
	verifyProcIdent func(pid int, expectedStartTime uint64) bool
}

// Deps bundles all wiring. Constructor takes a single struct so future
// additions don't break call sites.
type Deps struct {
	Cache        CacheView
	NodeAccess   NodeAccessor
	NodeCache    *node.CacheManager
	ClaudeDir    string
	Router       SessionRouter
	AllowedRoot  string
	DefaultAgent session.AgentOpts
	Broadcast    func()
	ValidateWS   func(ws, root string) (string, error)
	VerifyProcID func(pid int, expectedStartTime uint64) bool
}

// New constructs a Handlers from injected deps.
func New(d Deps) *Handlers {
	return &Handlers{
		cache:           d.Cache,
		nodeAccess:      d.NodeAccess,
		nodeCache:       d.NodeCache,
		claudeDir:       d.ClaudeDir,
		router:          d.Router,
		allowedRoot:     d.AllowedRoot,
		defaultAgent:    d.DefaultAgent,
		broadcast:       d.Broadcast,
		validateWS:      d.ValidateWS,
		verifyProcIdent: d.VerifyProcID,
	}
}

// SetAppContext is called once after the server context is available.
// Background takeover/close goroutines use this so they outlive the
// request and only die at process shutdown.
func (h *Handlers) SetAppContext(ctx context.Context) {
	h.appCtx = ctx
}

// Wait blocks until all background takeover/close goroutines have exited.
// Called from the server shutdown sequence after srv.Shutdown returns (no
// new requests can spawn goroutines at that point), so a graceful shutdown
// drains in-flight WaitAndCleanup work instead of leaving goroutines parked
// on FindProcess/exit waits after the HTTP server goroutine has gone away.
func (h *Handlers) Wait() {
	h.bg.Wait()
}

// SetClaudeDirForTest swaps claudeDir for tests that previously poked the
// unexported field directly. NOT for production use — the field is
// otherwise immutable after New().
func (h *Handlers) SetClaudeDirForTest(dir string) {
	h.claudeDir = dir
}

// HandleList serves GET /api/discovered — list discovered external CLI sessions.
func (h *Handlers) HandleList(w http.ResponseWriter, r *http.Request) {
	sessions := h.cache.Snapshot()

	// Merge remote discovered sessions
	if h.nodeAccess.HasNodes() {
		for i := range sessions {
			sessions[i].Node = "local"
		}
		cachedDiscovered := h.nodeCache.Discovered()
		allDiscovered := make([]any, 0, len(sessions))
		for _, d := range sessions {
			allDiscovered = append(allDiscovered, d)
		}
		for _, items := range cachedDiscovered {
			for _, item := range items {
				allDiscovered = append(allDiscovered, item)
			}
		}
		httputil.WriteJSON(w, allDiscovered)
		return
	}

	httputil.WriteJSON(w, sessions)
}

// HandlePreview serves GET /api/discovered/preview — preview a discovered
// session's history.
func (h *Handlers) HandlePreview(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
	nodeID := r.URL.Query().Get("node")
	if sessionID == "" || !discovery.IsValidSessionID(sessionID) {
		httputil.WriteJSON(w, []any{})
		return
	}

	// Remote node — only fall through to local when nodeID is empty or "local".
	if nodeID != "" && nodeID != "local" {
		// LookupNode validates nodeID against the allowlist ([a-zA-Z0-9._-],
		// 64-byte cap) and writes a 400 on failure, matching every other
		// remote-proxy handler. GetNode alone would let a log-injection
		// payload (\n, ANSI escapes) into the "node not connected" warn
		// attribute, which corrupts slog JSON output. R67-SEC-2.
		nc, ok := h.nodeAccess.LookupNode(w, nodeID)
		if !ok {
			return
		}
		entries, err := nc.FetchDiscoveredPreview(r.Context(), sessionID)
		if err != nil {
			slog.Warn("remote discovered preview", "node", nodeID, "err", err)
			entries = nil
		}
		if entries == nil {
			entries = []cli.EventEntry{}
		}
		httputil.WriteJSON(w, entries)
		return
	}

	// Local
	if h.claudeDir == "" {
		httputil.WriteJSON(w, []any{})
		return
	}

	// cwd is an optional optimisation hint. When present and valid it lets
	// LoadHistory resolve the JSONL via an O(1) os.Stat on the CWD-derived
	// path, bypassing the findSessionJSONL fallback scan AND its 60s
	// negative-result cache. The negative cache is what made interactive
	// preview flake: a single miss (card shown during the noJSONLGrace window
	// before the JSONL flushed, or while claude renamed it during history
	// compaction) poisons every preview for that session for 60s, rendering a
	// blank "暂无会话历史" splash that "fixes itself" only once the TTL
	// expires. The CWD direct-stat lookup runs before the cache check, so a
	// fresh poll picks the conversation up the instant it lands on disk.
	//
	// A stale/invalid hint degrades to "" (full scan) rather than erroring —
	// cwd never widens the result set, it only short-circuits the lookup.
	// Reject traversal / control-byte payloads so a crafted cwd cannot probe
	// arbitrary projDirName slugs (defense-in-depth; matches the takeover path
	// which validates CWD the same way).
	cwd := r.URL.Query().Get("cwd")
	if cwd != "" {
		if err := session.ValidateRemoteWorkspacePath(cwd); err != nil {
			cwd = ""
		}
	}

	entries, err := discovery.LoadHistory(h.claudeDir, sessionID, cwd)
	if err != nil {
		slog.Warn("preview load history", "session_id", sessionID, "err", err)
		entries = nil
	}
	if entries == nil {
		entries = []cli.EventEntry{}
	}

	httputil.WriteJSON(w, entries)
}

// HandleTakeover serves POST /api/discovered/takeover — kill an external CLI
// process and resume its session.
func (h *Handlers) HandleTakeover(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PID           int    `json:"pid"`
		SessionID     string `json:"session_id"`
		CWD           string `json:"cwd"`
		ProcStartTime uint64 `json:"proc_start_time"`
		Node          string `json:"node"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.PID <= 0 || req.SessionID == "" || !discovery.IsValidSessionID(req.SessionID) {
		http.Error(w, "pid and session_id are required", http.StatusBadRequest)
		return
	}

	// Remote node proxy
	if req.Node != "" && req.Node != "local" {
		nc, ok := h.nodeAccess.LookupNode(w, req.Node)
		if !ok {
			return
		}
		remoteKey, err := nc.ProxyTakeover(r.Context(), req.PID, req.SessionID, req.CWD, req.ProcStartTime)
		if err != nil {
			slog.Warn("proxy takeover failed", "node", req.Node, "err", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		httputil.WriteJSONStatus(w, http.StatusAccepted, map[string]string{"status": "accepted", "key": remoteKey, "node": req.Node})
		return
	}

	// Verify PID is in the discovered list before killing.
	// Use cache snapshot — fresh Scan() filters out dead processes.
	//
	// When claudeDir is empty there is no discovered list to cross-check
	// against, so an authenticated caller could otherwise submit any
	// positive pid+proc_start_time and SIGTERM arbitrary processes owned
	// by the naozhi user. Refuse the operation — matches handleClose's
	// 503 behaviour when claudeDir is unavailable. R67-SEC-4.
	if h.claudeDir == "" {
		http.Error(w, "discovery not available", http.StatusServiceUnavailable)
		return
	}
	cached := h.cache.Snapshot()
	pidFound := false
	for _, d := range cached {
		if d.PID == req.PID && d.SessionID == req.SessionID {
			pidFound = true
			break
		}
	}
	if !pidFound {
		http.Error(w, "pid not found in discovered sessions", http.StatusBadRequest)
		return
	}

	// Compute session key before launching goroutine so we can return it immediately.
	cwd := req.CWD
	if cwd == "" {
		cwd = "unknown"
	}
	// Validate CWD against allowedRoot to prevent sessions running in arbitrary directories.
	if cwd != "unknown" {
		// Reject `..` traversal segments and control bytes BEFORE
		// filepath.Clean — Clean collapses `/home/../etc` into `/etc`
		// silently, so a pure post-Clean check would let traversal slip
		// through as a now-canonical absolute path when allowedRoot is
		// empty (single-user default). session.ValidateRemoteWorkspacePath
		// encodes the same rules used on the remote-proxy path. R67-SEC-7.
		if err := session.ValidateRemoteWorkspacePath(cwd); err != nil {
			http.Error(w, "invalid cwd", http.StatusBadRequest)
			return
		}
		cwd = filepath.Clean(cwd)
		if h.allowedRoot != "" {
			if _, err := h.validateWS(cwd, h.allowedRoot); err != nil {
				http.Error(w, "cwd outside allowed root", http.StatusBadRequest)
				return
			}
		}
	}
	cwdKey := session.SanitizeCWDKey(cwd)
	key := session.TakeoverKey(cwdKey)

	// Kill the original process.
	// Verify PID identity before sending signal (TOCTOU guard).
	if req.ProcStartTime == 0 {
		http.Error(w, "proc_start_time is required", http.StatusBadRequest)
		return
	}
	alive := osutil.PidAlive(req.PID)
	if alive {
		if !h.verifyProcIdent(req.PID, req.ProcStartTime) {
			http.Error(w, "process identity changed (PID reused)", http.StatusConflict)
			return
		}
		if err := osutil.SendTerm(req.PID); err != nil {
			if !errors.Is(err, syscall.ESRCH) {
				slog.Error("failed to terminate process", "pid", req.PID, "err", err)
				http.Error(w, "failed to terminate process", http.StatusInternalServerError)
				return
			}
		}
	}

	// Immediately remove the killed PID from the discovery cache so the
	// frontend's fetchSessions() call (triggered right after the 202 response)
	// won't see the stale entry and re-render the old card in the sidebar.
	h.cache.EvictPID(req.PID)

	// Capture locals for the background goroutine.
	pid := req.PID
	sessionID := req.SessionID
	reqCWD := req.CWD
	procStartTime := req.ProcStartTime
	agentOpts := h.defaultAgent

	broadcast := h.broadcast
	claudeDir := h.claudeDir
	router := h.router

	h.bg.Add(1)
	go func() {
		defer h.bg.Done()
		// Wait, SIGKILL, and remove stale session files.
		discovery.WaitAndCleanup(h.appCtx, pid, procStartTime, claudeDir, reqCWD, sessionID)

		// Takeover via router — use Background context so the spawned process
		// outlives the HTTP request.
		err := router.Takeover(h.appCtx, key, sessionID, cwd, session.AgentOpts{
			Model:     agentOpts.Model,
			ExtraArgs: agentOpts.ExtraArgs,
		})
		if err != nil {
			slog.Error("session takeover failed", "key", key, "session_id", sessionID, "pid", pid, "err", err)
			if broadcast != nil {
				broadcast()
			}
			return
		}

		slog.Info("session takeover", "key", key, "session_id", sessionID, "pid", pid, "cwd", cwd)
		if broadcast != nil {
			broadcast()
		}
	}()

	httputil.WriteJSONStatus(w, http.StatusAccepted, map[string]string{"status": "accepted", "key": key})
}

// HandleClose serves POST /api/discovered/close — kill an external CLI process
// without resuming its session.
func (h *Handlers) HandleClose(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PID           int    `json:"pid"`
		SessionID     string `json:"session_id"`
		CWD           string `json:"cwd"`
		ProcStartTime uint64 `json:"proc_start_time"`
		Node          string `json:"node"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.PID <= 0 {
		http.Error(w, "pid is required", http.StatusBadRequest)
		return
	}

	// Remote node proxy
	if req.Node != "" && req.Node != "local" {
		nc, ok := h.nodeAccess.LookupNode(w, req.Node)
		if !ok {
			return
		}
		if err := nc.ProxyCloseDiscovered(r.Context(), req.PID, req.SessionID, req.CWD, req.ProcStartTime); err != nil {
			slog.Warn("proxy close discovered failed", "node", req.Node, "err", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		httputil.WriteOK(w)
		return
	}

	// Verify PID is in the discovered list before killing.
	// Use the cache snapshot instead of a fresh Scan(), because Scan()
	// filters out dead processes — if the process was already killed
	// externally the fresh scan won't find it, but we still need to
	// clean up the stale entry.
	if h.claudeDir == "" {
		http.Error(w, "discovery not available", http.StatusServiceUnavailable)
		return
	}
	cached := h.cache.Snapshot()
	var found *discovery.DiscoveredSession
	for i := range cached {
		if cached[i].PID == req.PID {
			found = &cached[i]
			break
		}
	}
	if found == nil {
		http.Error(w, "pid not found in discovered sessions", http.StatusBadRequest)
		return
	}
	// Use the cached SessionID/CWD for cleanup so a caller cannot
	// supply a crafted value to delete arbitrary session files.
	sessionID := found.SessionID
	cwd := found.CWD

	if req.ProcStartTime == 0 {
		http.Error(w, "proc_start_time is required", http.StatusBadRequest)
		return
	}

	// If the process is already dead, skip identity check and signal —
	// just do cleanup.  Otherwise verify PID identity and send SIGTERM.
	alive := osutil.PidAlive(req.PID)
	if alive {
		if !h.verifyProcIdent(req.PID, req.ProcStartTime) {
			http.Error(w, "process identity changed (PID reused)", http.StatusConflict)
			return
		}
		if err := osutil.SendTerm(req.PID); err != nil {
			// ESRCH = process disappeared between alive check and kill — treat as success.
			if !errors.Is(err, syscall.ESRCH) {
				slog.Error("failed to terminate process", "pid", req.PID, "err", err)
				http.Error(w, "failed to terminate process", http.StatusInternalServerError)
				return
			}
		}
	} else {
		slog.Info("discovered session already dead, cleaning up", "pid", req.PID)
	}

	// Evict from cache immediately so the frontend won't see the stale entry.
	h.cache.EvictPID(req.PID)

	// Background cleanup: wait for exit, SIGKILL if stuck, remove stale files.
	pid := req.PID
	procStartTime := req.ProcStartTime
	claudeDir := h.claudeDir
	broadcast := h.broadcast

	h.bg.Add(1)
	go func() {
		defer h.bg.Done()
		discovery.WaitAndCleanup(h.appCtx, pid, procStartTime, claudeDir, cwd, sessionID)
		slog.Info("discovered session closed", "pid", pid, "session_id", sessionID)
		if broadcast != nil {
			broadcast()
		}
	}()

	httputil.WriteOK(w)
}
