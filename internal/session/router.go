package session

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
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
		sessions:        make(map[string]*ManagedSession),
		wrapper:         cfg.Wrapper,
		maxProcs:        cfg.MaxProcs,
		ttl:             cfg.TTL,
		model:           cfg.Model,
		extraArgs:       cfg.ExtraArgs,
		storePath:       cfg.StorePath,
		noOutputTimeout: cfg.NoOutputTimeout,
		totalTimeout:    cfg.TotalTimeout,
	}

	// Restore sessions from store
	if restored := loadStore(r.storePath); restored != nil {
		for key, sessionID := range restored {
			r.sessions[key] = &ManagedSession{
				Key:       key,
				SessionID: sessionID,
			}
		}
	}
	return r
}

// AgentOpts provides per-agent overrides for session creation.
type AgentOpts struct {
	Model     string
	ExtraArgs []string
}

// GetOrCreate returns an existing session or creates a new one.
// AgentOpts overrides the router defaults for model and args.
func (r *Router) GetOrCreate(ctx context.Context, key string, opts AgentOpts) (*ManagedSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if s, ok := r.sessions[key]; ok {
		if s.process != nil && s.process.Alive() {
			s.LastActive = time.Now()
			return s, nil
		}
		slog.Info("session process dead, resuming", "key", key, "session_id", s.SessionID)
		return r.spawnSession(ctx, key, s.SessionID, opts)
	}

	slog.Info("creating new session", "key", key)
	return r.spawnSession(ctx, key, "", opts)
}

// spawnSession creates a new process, optionally resuming an existing session.
// Caller must hold r.mu.
func (r *Router) spawnSession(ctx context.Context, key string, resumeID string, opts AgentOpts) (*ManagedSession, error) {
	r.countActive()
	if r.activeCount >= r.maxProcs {
		if !r.evictOldest() {
			return nil, fmt.Errorf("max concurrent processes reached (%d), all busy", r.maxProcs)
		}
		r.countActive()
		if r.activeCount >= r.maxProcs {
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

	proc, err := r.wrapper.Spawn(ctx, cli.SpawnOptions{
		Model:           model,
		ResumeID:        resumeID,
		ExtraArgs:       args,
		NoOutputTimeout: r.noOutputTimeout,
		TotalTimeout:    r.totalTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("spawn process: %w", err)
	}

	s := &ManagedSession{
		Key:        key,
		SessionID:  resumeID,
		process:    proc,
		LastActive: time.Now(),
		sendMu:     sync.Mutex{},
	}
	r.sessions[key] = s
	r.activeCount++

	slog.Info("session spawned", "key", key, "active", r.activeCount)
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
		if oldest == nil || s.LastActive.Before(oldest.LastActive) {
			oldest = s
		}
	}
	if oldest == nil {
		return false
	}
	slog.Info("evicting oldest session", "key", oldest.Key, "idle", time.Since(oldest.LastActive))
	// Detach process before releasing lock so no other goroutine selects it
	proc := oldest.process
	oldest.process = nil
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
func (r *Router) Cleanup() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	for key, s := range r.sessions {
		if s.process != nil && s.process.Alive() && now.Sub(s.LastActive) > r.ttl {
			slog.Info("session expired", "key", key, "idle", now.Sub(s.LastActive))
			s.process.Close()
			r.activeCount--
			// Keep the session entry (with SessionID) for resume
		}
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
	r.mu.Lock()
	defer r.mu.Unlock()

	// Wait for running sessions to complete (up to 30s)
	deadline := time.Now().Add(30 * time.Second)
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
		time.Sleep(500 * time.Millisecond)
		r.mu.Lock()
	}

	// Save session state before closing
	if err := saveStore(r.storePath, r.sessions); err != nil {
		slog.Error("save session store on shutdown", "err", err)
	}

	for key, s := range r.sessions {
		if s.process != nil && s.process.Alive() {
			slog.Info("shutting down session", "key", key)
			s.process.Close()
		}
	}
}

// Stats returns current session statistics.
func (r *Router) Stats() (active, total int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.countActive()
	return r.activeCount, len(r.sessions)
}
