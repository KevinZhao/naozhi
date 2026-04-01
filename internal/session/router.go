package session

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/discovery"
)

const shutdownTimeout = 30 * time.Second

// Router manages session key -> ManagedSession mapping.
type Router struct {
	mu           sync.Mutex
	shutdownCond *sync.Cond // signaled when process state changes; conditioned on mu
	sessions     map[string]*ManagedSession
	// sessionsByChat is a secondary index: chat key → session keys.
	// Enables O(k) ResetChat instead of O(n) full scan (k = agents per chat, typically 1-3).
	// Nil in test-created routers; all helpers below are nil-safe.
	sessionsByChat map[string][]string
	wrapper        *cli.Wrapper
	maxProcs       int
	ttl            time.Duration
	model          string
	extraArgs      []string
	workspace      string // default cwd for CLI processes
	claudeDir      string // ~/.claude dir for loading session history

	// workspaceOverrides stores per-chat workspace overrides.
	// Key format: "platform:chatType:chatID"
	workspaceOverrides map[string]string

	// activeCount tracks currently alive processes
	activeCount int

	// pendingSpawns tracks Spawn() calls in progress (lock released during spawn)
	pendingSpawns int

	storePath       string
	noOutputTimeout time.Duration
	totalTimeout    time.Duration

	onChange func() // called (outside lock) when session list changes
}

// chatKeyFor strips the last ":agentID" segment from a session key to get the chat key.
func chatKeyFor(key string) string {
	if idx := strings.LastIndexByte(key, ':'); idx >= 0 {
		return key[:idx]
	}
	return key
}

// indexAdd adds key to the chat→sessions index. No-op when index is nil.
// Must be called under r.mu.
func (r *Router) indexAdd(key string) {
	if r.sessionsByChat == nil {
		return
	}
	ck := chatKeyFor(key)
	for _, k := range r.sessionsByChat[ck] {
		if k == key {
			return
		}
	}
	r.sessionsByChat[ck] = append(r.sessionsByChat[ck], key)
}

// indexDel removes key from the chat→sessions index. No-op when index is nil.
// Must be called under r.mu.
func (r *Router) indexDel(key string) {
	if r.sessionsByChat == nil {
		return
	}
	ck := chatKeyFor(key)
	keys := r.sessionsByChat[ck]
	for i, k := range keys {
		if k == key {
			last := len(keys) - 1
			keys[i] = keys[last]
			r.sessionsByChat[ck] = keys[:last]
			if len(r.sessionsByChat[ck]) == 0 {
				delete(r.sessionsByChat, ck)
			}
			return
		}
	}
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
	ClaudeDir       string
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
		sessionsByChat:     make(map[string][]string),
		wrapper:            cfg.Wrapper,
		maxProcs:           cfg.MaxProcs,
		ttl:                cfg.TTL,
		model:              cfg.Model,
		extraArgs:          cfg.ExtraArgs,
		workspace:          cfg.Workspace,
		claudeDir:          cfg.ClaudeDir,
		workspaceOverrides: make(map[string]string),
		storePath:          cfg.StorePath,
		noOutputTimeout:    cfg.NoOutputTimeout,
		totalTimeout:       cfg.TotalTimeout,
	}
	r.shutdownCond = sync.NewCond(&r.mu)

	// Restore sessions from store
	if restored := loadStore(r.storePath); restored != nil {
		for key, entry := range restored {
			s := &ManagedSession{
				Key:       key,
				workspace: entry.Workspace,
				totalCost: entry.TotalCost,
				Exempt:    strings.HasPrefix(key, "project:"),
			}
			s.setSessionID(entry.SessionID)
			r.sessions[key] = s
			r.indexAdd(key)
		}
		// Async-load JSONL history for suspended sessions so the dashboard
		// shows conversation history without waiting for the next message.
		if r.claudeDir != "" {
			for _, s := range r.sessions {
				s := s
				go func() {
					sid := s.getSessionID()
					if sid == "" {
						return
					}
					entries, err := discovery.LoadHistory(r.claudeDir, sid, s.workspace)
					if err != nil || len(entries) == 0 {
						return
					}
					s.InjectHistory(entries)
					slog.Info("loaded suspended session history on startup", "key", s.Key, "entries", len(entries))
					r.notifyChange()
				}()
			}
		}
	}
	return r
}

// SetOnChange registers a callback invoked when the session list changes.
func (r *Router) SetOnChange(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onChange = fn
}

// notifyChange calls the onChange callback if set. Must be called outside r.mu.
func (r *Router) notifyChange() {
	r.mu.Lock()
	fn := r.onChange
	r.mu.Unlock()
	if fn != nil {
		fn()
	}
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
	if r.sessionsByChat != nil {
		// O(k) path via index (k = agents per chat, typically 1-3).
		for _, key := range r.sessionsByChat[chatKeyPrefix] {
			s := r.sessions[key]
			if s == nil {
				continue
			}
			if s.process != nil && s.process.Alive() {
				toClose = append(toClose, s.process)
			}
			delete(r.sessions, key)
		}
		delete(r.sessionsByChat, chatKeyPrefix)
	} else {
		// Fallback O(n) scan for test-created routers without index.
		var toDelete []string
		for key, s := range r.sessions {
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
	}
	r.mu.Unlock()

	for _, proc := range toClose {
		proc.Close()
	}
	if r.shutdownCond != nil {
		r.shutdownCond.Broadcast()
	}

	r.mu.Lock()
	r.countActive()
	r.mu.Unlock()
	r.notifyChange()
}

// AgentOpts provides per-agent overrides for session creation.
type AgentOpts struct {
	Model     string
	ExtraArgs []string
	Workspace string // override workspace (empty = use default/chat override)
	Exempt    bool   // exempt from TTL, eviction, and activeCount (planner sessions)
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
	// Exempt sessions (planners) bypass maxProcs capacity check
	if !opts.Exempt {
		r.countActive()
		if r.activeCount+r.pendingSpawns >= r.maxProcs {
			if !r.evictOldest() {
				r.mu.Unlock()
				return nil, fmt.Errorf("max concurrent processes reached (%d), all busy", r.maxProcs)
			}
			r.countActive()
			if r.activeCount+r.pendingSpawns >= r.maxProcs {
				r.mu.Unlock()
				return nil, fmt.Errorf("max concurrent processes reached (%d), all busy", r.maxProcs)
			}
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

	// Determine workspace: opts override > per-chat override > old session workspace > default
	workspace := r.workspace
	workspaceOverridden := false
	if opts.Workspace != "" {
		workspace = opts.Workspace
		workspaceOverridden = true
	} else if chatKey := chatKeyFor(key); chatKey != key {
		if ws, ok := r.workspaceOverrides[chatKey]; ok {
			workspace = ws
			workspaceOverridden = true
		}
	}
	// When resuming after restart, workspaceOverrides is empty (not persisted across restarts).
	// Fall back to the old session's stored workspace so --resume finds the session in the
	// correct project directory (Claude stores sessions under ~/.claude/projects/<sha256(cwd)>/).
	if !workspaceOverridden && resumeID != "" {
		if old := r.sessions[key]; old != nil && old.workspace != "" {
			workspace = old.workspace
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
	r.pendingSpawns++
	r.mu.Unlock()
	if r.wrapper == nil {
		r.mu.Lock()
		r.pendingSpawns--
		r.mu.Unlock()
		return nil, fmt.Errorf("spawn process: no CLI wrapper configured")
	}
	proc, err := r.wrapper.Spawn(ctx, spawnOpts)
	r.mu.Lock()
	r.pendingSpawns--
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

	// Get old session reference, then release r.mu to copy history under sendMu only
	old := r.sessions[key]
	r.mu.Unlock()

	var oldHistory []cli.EventEntry
	if old != nil {
		old.sendMu.Lock()
		if old.process != nil && !old.process.Alive() {
			// Dead process: EventEntries() includes both injected history and live events
			// logged during the last run. Use this instead of persistedHistory, which only
			// holds the JSONL-loaded snapshot and misses events accumulated since that load.
			oldHistory = old.process.EventEntries()
		} else if len(old.persistedHistory) > 0 {
			oldHistory = make([]cli.EventEntry, len(old.persistedHistory))
			copy(oldHistory, old.persistedHistory)
		}
		old.sendMu.Unlock()
	}

	r.mu.Lock()
	// Re-check TOCTOU after re-acquiring lock (another goroutine may have spawned)
	if existing, ok := r.sessions[key]; ok && existing.process != nil && existing.process.Alive() {
		r.mu.Unlock()
		proc.Close()
		return existing, nil
	}

	s := &ManagedSession{
		Key:              key,
		process:          proc,
		workspace:        workspace,
		sendMu:           sync.Mutex{},
		persistedHistory: oldHistory,
		Exempt:           opts.Exempt,
	}
	if len(oldHistory) > 0 {
		proc.InjectHistory(oldHistory)
	}
	s.setSessionID(resumeID)
	s.touchLastActive()
	r.sessions[key] = s
	r.indexAdd(key)
	if !opts.Exempt {
		r.activeCount++
	}

	slog.Info("session spawned", "key", key, "active", r.activeCount, "exempt", opts.Exempt)
	r.mu.Unlock()

	// Load conversation history from Claude's local JSONL when resuming.
	// This restores dashboard event display after service restarts.
	if resumeID != "" && r.claudeDir != "" && len(oldHistory) == 0 {
		if entries, err := discovery.LoadHistory(r.claudeDir, resumeID, workspace); err == nil && len(entries) > 0 {
			s.InjectHistory(entries)
			slog.Info("loaded session history on resume", "key", key, "entries", len(entries))
		}
	}

	r.notifyChange()
	return s, nil
}

// countActive recounts alive processes (corrects drift from undetected exits).
// Exempt sessions are not counted toward max_procs capacity.
func (r *Router) countActive() {
	count := 0
	for _, s := range r.sessions {
		if s.Exempt {
			continue
		}
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
		if s.Exempt {
			continue // planner sessions are never evicted
		}
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
	oldest.deathReason.Store("evicted")
	// Keep oldest.process non-nil so concurrent holders don't get nil-panic.
	// After Close(), Alive() returns false — countActive() will stop counting it.
	proc := oldest.process
	r.mu.Unlock()
	proc.Close()
	if r.shutdownCond != nil {
		r.shutdownCond.Broadcast()
	}
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
	r.indexDel(key)
	delete(r.sessions, key)
	r.mu.Unlock()

	if proc != nil && proc.Alive() {
		proc.Close()
	}
	if r.shutdownCond != nil {
		r.shutdownCond.Broadcast()
	}

	r.mu.Lock()
	r.countActive()
	r.mu.Unlock()
	slog.Info("session reset", "key", key)
	r.notifyChange()
}

// Remove removes a session from the router without killing its process.
// Used by the dashboard to hide sessions from the list.
func (r *Router) Remove(key string) bool {
	r.mu.Lock()
	s, ok := r.sessions[key]
	if !ok {
		r.mu.Unlock()
		return false
	}

	// Kill process if alive
	proc := s.process
	r.indexDel(key)
	delete(r.sessions, key)
	r.mu.Unlock()

	if proc != nil && proc.Alive() {
		proc.Close()
	}
	if r.shutdownCond != nil {
		r.shutdownCond.Broadcast()
	}

	r.mu.Lock()
	r.countActive()
	r.mu.Unlock()
	slog.Info("session removed", "key", key)
	r.notifyChange()
	return true
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
		if s.Exempt {
			continue // planner sessions are never expired by TTL
		}
		if s.process != nil && s.process.Alive() && !s.process.IsRunning() && now.Sub(s.GetLastActive()) > r.ttl {
			slog.Info("session expired", "key", key, "idle", now.Sub(s.GetLastActive()))
			s.deathReason.Store("idle_timeout")
			expired = append(expired, expiredEntry{key, s.process})
		}
	}
	r.mu.Unlock()

	for _, e := range expired {
		e.proc.Close()
	}
	if r.shutdownCond != nil {
		r.shutdownCond.Broadcast()
	}

	r.mu.Lock()
	r.countActive()

	// Prune orphaned sessions: nil process, no session ID, past TTL
	var pruned int
	now2 := time.Now()
	for key, s := range r.sessions {
		if s.Exempt {
			continue // planner sessions are never pruned
		}
		if s.process == nil && s.getSessionID() == "" && now2.Sub(s.GetLastActive()) > r.ttl {
			r.indexDel(key)
			delete(r.sessions, key)
			pruned++
			continue
		}
		// Prune dead sessions with no resumable session ID
		if s.process != nil && !s.process.Alive() && s.getSessionID() == "" && now2.Sub(s.GetLastActive()) > r.ttl {
			r.indexDel(key)
			delete(r.sessions, key)
			pruned++
			continue
		}
		// Prune old dead sessions even with session ID (prevents unbounded growth)
		if s.process != nil && !s.process.Alive() && now2.Sub(s.GetLastActive()) > 7*r.ttl {
			r.indexDel(key)
			delete(r.sessions, key)
			pruned++
		}
	}
	r.mu.Unlock()

	if len(expired) > 0 || pruned > 0 {
		r.notifyChange()
	}
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
	// Deadline goroutine: broadcast to unblock Wait() when timeout expires
	go func() {
		time.Sleep(shutdownTimeout)
		if r.shutdownCond != nil {
			r.shutdownCond.Broadcast()
		}
	}()

	r.mu.Lock()

	// Wait for running sessions to complete (up to shutdownTimeout)
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
		if r.shutdownCond != nil {
			r.shutdownCond.Wait() // atomically releases and re-acquires r.mu
		} else {
			// Fallback for tests without shutdownCond
			r.mu.Unlock()
			time.Sleep(100 * time.Millisecond)
			r.mu.Lock()
		}
	}

	// Snapshot sessions for saving outside lock
	sessionsCopy := make(map[string]*ManagedSession, len(r.sessions))
	for k, v := range r.sessions {
		sessionsCopy[k] = v
	}
	storePath := r.storePath

	// Collect processes to close, then release lock to close concurrently
	var procs []processIface
	for key, s := range r.sessions {
		if s.process != nil && s.process.Alive() {
			slog.Info("shutting down session", "key", key)
			procs = append(procs, s.process)
		}
	}
	r.mu.Unlock()

	// Save session state outside lock (avoids JSON marshal + file I/O under mutex)
	if err := saveStore(storePath, sessionsCopy); err != nil {
		slog.Error("save session store on shutdown", "err", err)
	}

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

// DefaultWorkspace returns the router's default working directory.
func (r *Router) DefaultWorkspace() string {
	return r.workspace
}

// MaxProcs returns the maximum number of concurrent CLI processes.
func (r *Router) MaxProcs() int {
	return r.maxProcs
}

// Stats returns current session statistics.
// active = sessions with a live process (ready or running, excluding exempt);
// total = all sessions in the map including dead and suspended ones.
func (r *Router) Stats() (active, total int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.countActive() // keeps activeCount up-to-date for capacity management
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

// InterruptSession sends SIGINT to the CLI process for the given session key.
// Returns true if the session was found and interrupted.
func (r *Router) InterruptSession(key string) bool {
	r.mu.Lock()
	s := r.sessions[key]
	r.mu.Unlock()
	if s == nil {
		return false
	}
	return s.Interrupt()
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

// Takeover creates a managed session to replace an external Claude CLI session.
// It uses --resume to preserve the conversation context, and loads JSONL history
// for dashboard display. The caller must ensure the original process has been
// terminated before calling.
func (r *Router) Takeover(ctx context.Context, key string, sessionID string, workspace string, opts AgentOpts) (*ManagedSession, error) {
	r.mu.Lock()
	// If key already exists (e.g. re-takeover same CWD), close the old process
	if s, ok := r.sessions[key]; ok {
		if s.process != nil && s.process.Alive() {
			oldSession := s
			proc := s.process
			r.mu.Unlock()
			proc.Close()
			r.mu.Lock()
			// Only delete if no concurrent goroutine replaced this session
			if cur, ok := r.sessions[key]; ok && cur == oldSession {
				r.indexDel(key)
				delete(r.sessions, key)
			} else if cur != nil && cur.process != nil && cur.process.Alive() {
				// Concurrent GetOrCreate created a new session during Close();
				// abort takeover rather than silently returning wrong session.
				r.mu.Unlock()
				return nil, fmt.Errorf("concurrent session created for key %s during takeover", key)
			}
			// Implicit else: concurrent goroutine replaced the session with a dead
			// one. Leave r.sessions[key] as-is — spawnSession below will overwrite
			// it and call indexAdd, keeping the index consistent. No indexDel here
			// because we are not removing from r.sessions.
		} else {
			r.indexDel(key)
			delete(r.sessions, key)
		}
		r.countActive()
	}
	// Set workspace override for the chat key prefix
	if chatKey := chatKeyFor(key); chatKey != key {
		r.workspaceOverrides[chatKey] = workspace
	}
	s, err := r.spawnSession(ctx, key, sessionID, opts)
	if err != nil {
		return nil, err
	}
	return s, nil
}
