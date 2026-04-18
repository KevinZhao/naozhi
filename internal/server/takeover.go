package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"syscall"

	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/session"
)

// verifyProcIdentity checks that the process at pid still has the expected
// start time, guarding against PID reuse between scan and signal.
func verifyProcIdentity(pid int, expectedStartTime uint64) bool {
	actual, err := discovery.ProcStartTime(pid)
	if err != nil {
		return false
	}
	return actual == expectedStartTime
}

// killAndCleanupClaude terminates an external Claude CLI process and removes its
// stale session/lock files so the session can be cleanly resumed with --resume.
// Sequence: SIGTERM → wait up to 5 s → SIGKILL (only if PID identity still matches).
func (s *Server) killAndCleanupClaude(ctx context.Context, pid int, procStartTime uint64, cwd, sessionID string) error {
	// TOCTOU guard: reject if the PID was recycled since the discovery scan.
	if procStartTime != 0 && !verifyProcIdentity(pid, procStartTime) {
		return fmt.Errorf("process identity changed (PID reused): pid=%d", pid)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("sigterm pid %d: %w", pid, err)
	}
	discovery.WaitAndCleanup(ctx, pid, procStartTime, s.claudeDir, cwd, sessionID)
	return nil
}

// tryAutoTakeover looks for an external Claude CLI session whose CWD matches the
// chat's effective workspace and transparently adopts it under naozhi management.
// Must be called inside the sessionGuard critical section (after TryAcquire).
// Returns true when a session was successfully taken over.
func (s *Server) tryAutoTakeover(ctx context.Context, chatKey, key string, opts session.AgentOpts) bool {
	if s.claudeDir == "" {
		return false
	}
	// Skip when a managed session already exists for this key.
	if s.router.GetSession(key) != nil {
		return false
	}
	workspace := opts.Workspace
	if workspace == "" {
		workspace = s.router.GetWorkspace(chatKey)
	}
	if workspace == "" {
		return false
	}
	pids, sids, cwds := s.router.ManagedExcludeSets()
	discovered, err := discovery.Scan(s.claudeDir, pids, sids, cwds)
	if err != nil || len(discovered) == 0 {
		return false
	}
	// Find the most recently active session whose CWD matches the workspace.
	var best *discovery.DiscoveredSession
	for i := range discovered {
		ds := &discovered[i]
		if ds.CWD == workspace {
			if best == nil || ds.LastActive > best.LastActive {
				best = ds
			}
		}
	}
	if best == nil {
		return false
	}
	if err := s.killAndCleanupClaude(ctx, best.PID, best.ProcStartTime, best.CWD, best.SessionID); err != nil {
		slog.Warn("auto-takeover: kill failed", "key", key, "pid", best.PID, "err", err)
		return false
	}
	takeoverOpts := opts
	takeoverOpts.Workspace = best.CWD
	if _, err := s.router.Takeover(ctx, key, best.SessionID, best.CWD, takeoverOpts); err != nil {
		slog.Warn("auto-takeover: resume failed", "key", key, "session_id", best.SessionID, "err", err)
		return false
	}
	slog.Info("auto-takeover completed", "key", key, "pid", best.PID, "session_id", best.SessionID, "workspace", best.CWD)
	return true
}
