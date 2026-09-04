// Package discovery hosts the dashboard endpoints that surface external
// Claude CLI sessions on this host (and connected nodes) and let operators
// take them over or close them under naozhi management. Server dependencies
// are injected as closures / small interfaces (CacheView, NodeAccessor,
// SessionRouter) so the package never reverse-imports internal/server.
package discovery

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"
)

// CacheView is the subset of the server's discovery cache this package needs.
type CacheView interface {
	Snapshot() []discovery.DiscoveredSession
	EvictPID(pid int)
}

// NodeAccessor is the 2-method subset of contracts.NodeAccessor these handlers
// call; kept narrow for the test doubles, and asserted a strict subset by
// TestDashboardContractsDeclaredOnce (#2285).
type NodeAccessor interface {
	HasNodes() bool
	LookupNode(w http.ResponseWriter, id string) (node.Conn, bool)
}

// SessionRouter is the subset of *session.Router this package calls. Takeover
// drops the *ManagedSession return (only success/failure matters) via an
// adapter at the wiring site.
type SessionRouter interface {
	Takeover(ctx context.Context, key, sessionID, cwd string, opts session.AgentOpts) error
}

// Handlers groups the discovered-session and takeover API endpoints.
// Constructed once via New() before the server context exists;
// SetAppContext(ctx) is called afterwards so background takeover/close
// goroutines outlive the request but die at process shutdown.
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
	// procStartTime reads /proc start_time for a pid; feeds the pidfd-based
	// SendTermVerified guard so SIGTERM cannot leak to a recycled PID (#1670).
	procStartTime func(pid int) (uint64, error)
}

// Deps bundles all wiring for New.
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
	// ProcStartTime feeds the SendTermVerified identity guard (#1670).
	ProcStartTime func(pid int) (uint64, error)
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
		procStartTime:   d.ProcStartTime,
	}
}

// sendTermVerified routes SIGTERM through osutil.SendTermVerified, which pins
// the target via pidfd so the kill cannot leak to a recycled PID (#1670).
// When procStartTime is nil (older wiring / test doubles) the verifyProcIdent
// pre-check is adapted into the identity guard so no caller loses
// defence-in-depth.
func (h *Handlers) sendTermVerified(pid int, expectedStartTime uint64) error {
	stFn := h.procStartTime
	if stFn == nil && h.verifyProcIdent != nil {
		// Adapt the bool verifier into the (uint64, error) shape.
		stFn = func(p int) (uint64, error) {
			if h.verifyProcIdent(p, expectedStartTime) {
				return expectedStartTime, nil
			}
			return 0, osutil.ErrPidReused
		}
	}
	return osutil.SendTermVerified(pid, expectedStartTime, stFn)
}

// SetAppContext is called once after the server context exists; background
// takeover/close goroutines use it to outlive the request until shutdown.
func (h *Handlers) SetAppContext(ctx context.Context) {
	h.appCtx = ctx
}

// Wait blocks until all background takeover/close goroutines have exited.
// Called after srv.Shutdown returns so a graceful shutdown drains in-flight
// WaitAndCleanup work.
func (h *Handlers) Wait() {
	h.bg.Wait()
}

// SetClaudeDirForTest swaps claudeDir for tests. NOT for production use.
func (h *Handlers) SetClaudeDirForTest(dir string) {
	h.claudeDir = dir
}

// HandleList serves GET /api/discovered — list discovered external CLI sessions.
func (h *Handlers) HandleList(w http.ResponseWriter, r *http.Request) {
	sessions := h.cache.Snapshot()

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
		// LookupNode validates nodeID against the allowlist and writes a 400
		// on failure; GetNode alone would let a log-injection payload into the
		// warn attribute.
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
			entries = []clievent.EventEntry{}
		}
		httputil.WriteJSON(w, entries)
		return
	}

	if h.claudeDir == "" {
		httputil.WriteJSON(w, []any{})
		return
	}

	// cwd is an optional hint letting LoadHistory resolve the JSONL via an
	// O(1) stat, bypassing the findSessionJSONL scan and its 60s negative
	// cache (which otherwise made a single early miss blank the preview for
	// 60s). An invalid hint degrades to "" (full scan) — cwd never widens the
	// result set; traversal / control bytes are rejected as on the takeover path.
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
		entries = []clievent.EventEntry{}
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

	// Verify PID is in the discovered list (cache snapshot — a fresh Scan()
	// filters out dead processes). With claudeDir empty there is no list to
	// cross-check, so an authenticated caller could SIGTERM arbitrary
	// processes owned by the naozhi user: refuse, matching handleClose.
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

	// Compute the session key before launching the goroutine so we can return it.
	cwd := req.CWD
	if cwd == "" {
		cwd = "unknown"
	}
	if cwd != "unknown" {
		// Reject `..` and control bytes BEFORE filepath.Clean: Clean collapses
		// `/home/../etc` into `/etc`, so a post-Clean-only check would let
		// traversal through when allowedRoot is empty.
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

	if req.ProcStartTime == 0 {
		http.Error(w, "proc_start_time is required", http.StatusBadRequest)
		return
	}
	// Atomic identity-confirmed SIGTERM (#1670): pidfd pins the instance so no
	// PidAlive→verify→SendTerm window remains. ESRCH is success; ErrPidReused
	// is the 409 the frontend expects.
	if err := h.sendTermVerified(req.PID, req.ProcStartTime); err != nil {
		if errors.Is(err, osutil.ErrPidReused) {
			http.Error(w, "process identity changed (PID reused)", http.StatusConflict)
			return
		}
		if !errors.Is(err, syscall.ESRCH) {
			slog.Error("failed to terminate process", "pid", req.PID, "err", err)
			http.Error(w, "failed to terminate process", http.StatusInternalServerError)
			return
		}
	}

	// Evict the killed PID now so the frontend's immediate fetchSessions()
	// does not re-render the stale card.
	h.cache.EvictPID(req.PID)

	pid := req.PID
	sessionID := req.SessionID
	procStartTime := req.ProcStartTime
	agentOpts := h.defaultAgent

	broadcast := h.broadcast
	claudeDir := h.claudeDir
	router := h.router

	h.bg.Add(1)
	go func() {
		defer h.bg.Done()
		// Use the cleaned cwd so the lock-dir path WaitAndCleanup derives via
		// projDirName matches the cwd router.Takeover spawns under (#1786).
		discovery.WaitAndCleanup(h.appCtx, pid, procStartTime, claudeDir, cwd, sessionID)

		// appCtx so the spawned process outlives the HTTP request.
		err := router.Takeover(h.appCtx, key, sessionID, cwd, session.AgentOpts{
			Model:     agentOpts.Model,
			ExtraArgs: agentOpts.ExtraArgs,
			// Carry the agent's standing system prompt (#2493).
			SystemPrompt: agentOpts.SystemPrompt,
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

	// Verify PID is in the discovered list using the cache snapshot: a fresh
	// Scan() drops dead processes, but a stale entry still needs cleanup.
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

	// Atomic identity-confirmed SIGTERM (#1670): pidfd pins the exact original
	// instance or fails ESRCH, closing the PID-reuse TOCTOU window. ESRCH
	// (already dead) falls through to cleanup.
	if err := h.sendTermVerified(req.PID, req.ProcStartTime); err != nil {
		if errors.Is(err, osutil.ErrPidReused) {
			http.Error(w, "process identity changed (PID reused)", http.StatusConflict)
			return
		}
		if !errors.Is(err, syscall.ESRCH) {
			slog.Error("failed to terminate process", "pid", req.PID, "err", err)
			http.Error(w, "failed to terminate process", http.StatusInternalServerError)
			return
		}
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
