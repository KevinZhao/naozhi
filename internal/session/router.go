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
}

// RouterConfig holds configuration for the session router.
type RouterConfig struct {
	Wrapper   *cli.Wrapper
	MaxProcs  int
	TTL       time.Duration
	Model     string
	ExtraArgs []string
}

// NewRouter creates a session router.
func NewRouter(cfg RouterConfig) *Router {
	if cfg.MaxProcs <= 0 {
		cfg.MaxProcs = 3
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Minute
	}
	return &Router{
		sessions:  make(map[string]*ManagedSession),
		wrapper:   cfg.Wrapper,
		maxProcs:  cfg.MaxProcs,
		ttl:       cfg.TTL,
		model:     cfg.Model,
		extraArgs: cfg.ExtraArgs,
	}
}

// GetOrCreate returns an existing session or creates a new one.
func (r *Router) GetOrCreate(ctx context.Context, key string) (*ManagedSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if s, ok := r.sessions[key]; ok {
		if s.process != nil && s.process.Alive() {
			s.LastActive = time.Now()
			return s, nil
		}
		// Process dead, try to resume
		slog.Info("session process dead, resuming", "key", key, "session_id", s.SessionID)
		return r.spawnSession(ctx, key, s.SessionID)
	}

	// New session
	slog.Info("creating new session", "key", key)
	return r.spawnSession(ctx, key, "")
}

// spawnSession creates a new process, optionally resuming an existing session.
// Caller must hold r.mu.
func (r *Router) spawnSession(ctx context.Context, key string, resumeID string) (*ManagedSession, error) {
	// Check max procs
	r.countActive()
	if r.activeCount >= r.maxProcs {
		// Try to evict the oldest idle session
		if !r.evictOldest() {
			return nil, fmt.Errorf("max concurrent processes reached (%d), message queued", r.maxProcs)
		}
	}

	proc, err := r.wrapper.Spawn(ctx, cli.SpawnOptions{
		Model:     r.model,
		ResumeID:  resumeID,
		ExtraArgs: r.extraArgs,
	})
	if err != nil {
		return nil, fmt.Errorf("spawn process: %w", err)
	}

	s := &ManagedSession{
		Key:        key,
		SessionID:  resumeID, // may be empty, will be set after first Send
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

// evictOldest closes the oldest idle (Ready) session to free a slot.
// Returns true if a session was evicted.
func (r *Router) evictOldest() bool {
	var oldest *ManagedSession
	for _, s := range r.sessions {
		if s.process == nil || !s.process.Alive() {
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
	oldest.process.Close()
	r.activeCount--
	return true
}

// Reset discards the session for the given key (user sent /new).
func (r *Router) Reset(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if s, ok := r.sessions[key]; ok {
		if s.process != nil && s.process.Alive() {
			s.process.Close()
			r.activeCount--
		}
		delete(r.sessions, key)
		slog.Info("session reset", "key", key)
	}
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

// Shutdown gracefully closes all sessions.
func (r *Router) Shutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()

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
