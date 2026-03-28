package session

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

const (
	shutdownTimeout      = 30 * time.Second
	shutdownPollInterval = 500 * time.Millisecond
)

// Router manages session key -> ManagedSession mapping.
type Router struct {
	mu        sync.Mutex
	sessions  map[string]*ManagedSession
	wrapper   *cli.Wrapper
	maxProcs  int
	ttl       time.Duration
	model     string
	extraArgs []string
	workspace string // default cwd for CLI processes

	// workspaceOverrides stores per-chat workspace overrides.
	// Key format: "platform:chatType:chatID"
	workspaceOverrides map[string]string

	// activeCount tracks currently alive processes
	activeCount int

	storePath       string
	noOutputTimeout time.Duration
	totalTimeout    time.Duration
}

// RouterConfig holds configuration for the session router.
type RouterConfig struct {
	Wrapper         *cli.Wrapper
	MaxProcs        int
	TTL             time.Duration
	Model           string
	ExtraArgs       []string
	Workspace       string
	StorePath       string
	NoOutputTimeout time.Duration
	TotalTimeout    time.Duration
}

// NewRouter creates a session router.
func NewRouter(cfg RouterConfig) *Router {
	if cfg.MaxProcs <= 0 {
		cfg.MaxProcs = 3
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Minute
	}
	r := &Router{
		sessions:           make(map[string]*ManagedSession),
		wrapper:            cfg.Wrapper,
		maxProcs:           cfg.MaxProcs,
		ttl:                cfg.TTL,
		model:              cfg.Model,
		extraArgs:          cfg.ExtraArgs,
		workspace:          cfg.Workspace,
		workspaceOverrides: make(map[string]string),
		storePath:          cfg.StorePath,
		noOutputTimeout:    cfg.NoOutputTimeout,
		totalTimeout:       cfg.TotalTimeout,
	}

	// Restore sessions from store
	if restored := loadStore(r.storePath); restored != nil {
		for key, entry := range restored {
			s := &ManagedSession{
				Key:       key,
				workspace: entry.Workspace,
				totalCost: entry.TotalCost,
			}
			s.setSessionID(entry.SessionID)
			r.sessions[key] = s
		}
	}
	return r
}

// ChatKey builds a chat-level key (without agent suffix) for workspace overrides.
func ChatKey(platform, chatType, chatID string) string {
	return platform + ":" + chatType + ":" + chatID
}

// SetWorkspace sets the working directory override for a chat.
func (r *Router) SetWorkspace(chatKey, path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.workspaceOverrides[chatKey] = path
}

// GetWorkspace returns the effective workspace for a chat key.
func (r *Router) GetWorkspace(chatKey string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ws, ok := r.workspaceOverrides[chatKey]; ok {
		return ws
	}
	return r.workspace
}

// ResetChat resets all sessions belonging to a chat (all agents).
func (r *Router) ResetChat(chatKeyPrefix string) {
	r.mu.Lock()
	var toClose []processIface
	var toDelete []string
	for key, s := range r.sessions {
		// Session key: "platform:chatType:chatID:agentID"
		// Chat key:    "platform:chatType:chatID"
		// Match if session key starts with chatKey + ":"
		if len(key) > len(chatKeyPrefix) && key[:len(chatKeyPrefix)+1] == chatKeyPrefix+":" {
			toDelete = append(toDelete, key)
			if s.process != nil && s.process.Alive() {
				toClose = append(toClose, s.process)
			}
		}
	}
	for _, key := range toDelete {
		delete(r.sessions, key)
	}
	r.mu.Unlock()

	for _, proc := range toClose {
		proc.Close()
	}

	r.mu.Lock()
	r.countActive()
	r.mu.Unlock()
}

// AgentOpts provides per-agent overrides for session creation.
type AgentOpts struct {
	Model     string
	ExtraArgs []string
}

// SessionStatus indicates how a session was obtained.
type SessionStatus int

const (
	SessionExisting SessionStatus = iota // reused a live session
	SessionResumed                       // resumed a dead session
	SessionNew                           // created a brand new session
)

// GetOrCreate returns an existing session or creates a new one.
// AgentOpts overrides the router defaults for model and args.
func (r *Router) GetOrCreate(ctx context.Context, key string, opts AgentOpts) (*ManagedSession, SessionStatus, error) {
	r.mu.Lock()

	if s, ok := r.sessions[key]; ok {
		if s.process != nil && s.process.Alive() {
			s.touchLastActive()
			r.mu.Unlock()
			return s, SessionExisting, nil
		}
		slog.Info("session process dead, resuming", "key", key, "session_id", s.getSessionID())
		s, err := r.spawnSession(ctx, key, s.getSessionID(), opts)
		if err != nil {
			return nil, 0, err
		}
		return s, SessionResumed, nil
	}

	slog.Info("creating new session", "key", key)
	s, err := r.spawnSession(ctx, key, "", opts)
	if err != nil {
		return nil, 0, err
	}
	return s, SessionNew, nil
}

// spawnSession creates a new process, optionally resuming an existing session.
// Caller must hold r.mu. Releases r.mu during Spawn() to avoid blocking other
// goroutines during potentially slow protocol init (e.g., ACP handshake).
func (r *Router) spawnSession(ctx context.Context, key string, resumeID string, opts AgentOpts) (*ManagedSession, error) {
	r.countActive()
	if r.activeCount >= r.maxProcs {
		if !r.evictOldest() {
			r.mu.Unlock()
			return nil, fmt.Errorf("max concurrent processes reached (%d), all busy", r.maxProcs)
		}
		r.countActive()
		if r.activeCount >= r.maxProcs {
			r.mu.Unlock()
			return nil, fmt.Errorf("max concurrent processes reached (%d), all busy", r.maxProcs)
		}
	}

	// Merge agent opts with router defaults
	model := r.model
	if opts.Model != "" {
		model = opts.Model
	}
	args := make([]string, len(r.extraArgs))
	copy(args, r.extraArgs)
	args = append(args, opts.ExtraArgs...)

	// Determine workspace: check per-chat override first, then default
	workspace := r.workspace
	// Extract chat key prefix from session key (strip last :agentID segment)
	if idx := lastIndexByte(key, ':'); idx >= 0 {
		chatKey := key[:idx]
		if ws, ok := r.workspaceOverrides[chatKey]; ok {
			workspace = ws
		}
	}

	spawnOpts := cli.SpawnOptions{
		Model:           model,
		ResumeID:        resumeID,
		ExtraArgs:       args,
		WorkingDir:      workspace,
		NoOutputTimeout: r.noOutputTimeout,
		TotalTimeout:    r.totalTimeout,
	}

	// Release lock during Spawn (may block on ACP Init handshake)
	r.mu.Unlock()
	if r.wrapper == nil {
		r.mu.Lock()
		return nil, fmt.Errorf("spawn process: no CLI wrapper configured")
	}
	proc, err := r.wrapper.Spawn(ctx, spawnOpts)
	r.mu.Lock()
	if err != nil {
		r.mu.Unlock()
		return nil, fmt.Errorf("spawn process: %w", err)
	}

	// TOCTOU guard: another goroutine may have spawned this key while we were unlocked
	if existing, ok := r.sessions[key]; ok && existing.process != nil && existing.process.Alive() {
		r.mu.Unlock()
		proc.Close() // discard the redundant process
		return existing, nil
	}

	s := &ManagedSession{
		Key:       key,
		process:   proc,
		workspace: workspace,
		sendMu:    sync.Mutex{},
	}
	s.setSessionID(resumeID)
	s.touchLastActive()
	r.sessions[key] = s
	r.activeCount++

	slog.Info("session spawned", "key", key, "active", r.activeCount)
	r.mu.Unlock()
	return s, nil
}

// countActive recounts alive processes (corrects drift from undetected exits).
func (r *Router) countActive() {
	count := 0
	for _, s := range r.sessions {
		if s.process != nil && s.process.Alive() {
			count++
		}
	}
	r.activeCount = count
}

// evictOldest closes the oldest idle (non-Running) session to free a slot.
// Releases and re-acquires r.mu during Close() to avoid blocking other goroutines.
// Returns true if a session was evicted.
func (r *Router) evictOldest() bool {
	var oldest *ManagedSession
	for _, s := range r.sessions {
		if s.process == nil || !s.process.Alive() || s.process.IsRunning() {
			continue
		}
		if oldest == nil || s.GetLastActive().Before(oldest.GetLastActive()) {
			oldest = s
		}
	}
	if oldest == nil {
		return false
	}
	slog.Info("evicting oldest session", "key", oldest.Key, "idle", time.Since(oldest.GetLastActive()))
	oldest.deathReason = "evicted"
	// Keep oldest.process non-nil so concurrent holders don't get nil-panic.
	// After Close(), Alive() returns false — countActive() will stop counting it.
	proc := oldest.process
	r.mu.Unlock()
	proc.Close()
	r.mu.Lock()
	r.countActive()
	return true
}

// Reset discards the session for the given key (user sent /new).
func (r *Router) Reset(key string) {
	r.mu.Lock()

	s, ok := r.sessions[key]
	if !ok {
		r.mu.Unlock()
		return
	}

	proc := s.process
	delete(r.sessions, key)
	r.mu.Unlock()

	if proc != nil && proc.Alive() {
		proc.Close()
	}

	r.mu.Lock()
	r.countActive()
	r.mu.Unlock()
	slog.Info("session reset", "key", key)
}

// Cleanup closes sessions idle beyond TTL.
// Releases r.mu during Close() to avoid blocking message processing.
func (r *Router) Cleanup() {
	r.mu.Lock()

	type expiredEntry struct {
		key  string
		proc processIface
	}
	var expired []expiredEntry

	now := time.Now()
	for key, s := range r.sessions {
		if s.process != nil && s.process.Alive() && !s.process.IsRunning() && now.Sub(s.GetLastActive()) > r.ttl {
			slog.Info("session expired", "key", key, "idle", now.Sub(s.GetLastActive()))
			s.deathReason = "idle_timeout"
			expired = append(expired, expiredEntry{key, s.process})
		}
	}
	r.mu.Unlock()

	for _, e := range expired {
		e.proc.Close()
	}

	r.mu.Lock()
	r.countActive()
	r.mu.Unlock()
}

// StartCleanupLoop runs Cleanup periodically.
func (r *Router) StartCleanupLoop(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.Cleanup()
			}
		}
	}()
}

// Shutdown gracefully closes all sessions, waiting for running ones to complete.
func (r *Router) Shutdown() {
	r.mu.Lock()

	// Wait for running sessions to complete (up to 30s)
	deadline := time.Now().Add(shutdownTimeout)
	for {
		running := false
		for _, s := range r.sessions {
			if s.process != nil && s.process.IsRunning() {
				running = true
				break
			}
		}
		if !running || time.Now().After(deadline) {
			break
		}
		r.mu.Unlock()
		time.Sleep(shutdownPollInterval)
		r.mu.Lock()
	}

	// Save session state before closing
	if err := saveStore(r.storePath, r.sessions); err != nil {
		slog.Error("save session store on shutdown", "err", err)
	}

	// Collect processes to close, then release lock to close concurrently
	var procs []processIface
	for key, s := range r.sessions {
		if s.process != nil && s.process.Alive() {
			slog.Info("shutting down session", "key", key)
			procs = append(procs, s.process)
		}
	}
	r.mu.Unlock()

	var wg sync.WaitGroup
	for _, proc := range procs {
		wg.Add(1)
		go func(p processIface) {
			defer wg.Done()
			p.Close()
		}(proc)
	}
	wg.Wait()
}

func lastIndexByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// Stats returns current session statistics.
func (r *Router) Stats() (active, total int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.countActive()
	return r.activeCount, len(r.sessions)
}

// ListSessions returns a snapshot of all sessions for the dashboard.
// Collects references under r.mu, then releases before snapshotting
// to avoid blocking the router while getSessionID() waits on sendMu.
func (r *Router) ListSessions() []SessionSnapshot {
	r.mu.Lock()
	refs := make([]*ManagedSession, 0, len(r.sessions))
	for _, s := range r.sessions {
		refs = append(refs, s)
	}
	r.mu.Unlock()

	snapshots := make([]SessionSnapshot, 0, len(refs))
	for _, s := range refs {
		snapshots = append(snapshots, s.Snapshot())
	}
	return snapshots
}

// GetSession returns the session for the given key, or nil.
func (r *Router) GetSession(key string) *ManagedSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[key]
}

// ManagedPIDs returns the OS PIDs of all alive managed processes.
func (r *Router) ManagedPIDs() map[int]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	pids := make(map[int]bool)
	for _, s := range r.sessions {
		if s.process != nil && s.process.Alive() {
			if pid := s.process.PID(); pid > 0 {
				pids[pid] = true
			}
		}
	}
	return pids
}

// Takeover creates a managed session by resuming an external Claude CLI session.
// The caller must ensure the original process has been terminated before calling.
func (r *Router) Takeover(ctx context.Context, key string, sessionID string, workspace string, opts AgentOpts) (*ManagedSession, error) {
	r.mu.Lock()
	// If key already exists and alive, return it
	if s, ok := r.sessions[key]; ok && s.process != nil && s.process.Alive() {
		r.mu.Unlock()
		return s, nil
	}
	// Set workspace override for the chat key prefix
	if idx := lastIndexByte(key, ':'); idx >= 0 {
		chatKey := key[:idx]
		r.workspaceOverrides[chatKey] = workspace
	}
	s, err := r.spawnSession(ctx, key, sessionID, opts)
	if err != nil {
		return nil, err
	}
	s.workspace = workspace
	return s, nil
}
