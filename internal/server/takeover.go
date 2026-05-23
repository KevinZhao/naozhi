package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"syscall"

	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"
)

// verifyProcIdentity checks that the process at pid still has the expected
// start time, guarding against PID reuse between scan and signal.
//
// Background: Linux PIDs wrap around (default kernel.pid_max=4194304 on
// modern kernels but historically 32768) so the moment between
// discovery.Scan() and osutil.SendTerm() leaves a window where the
// original Claude process exits, the kernel recycles its PID for an
// unrelated process — possibly even a sandboxed system service — and
// our SIGTERM lands on the wrong target. /proc/<pid>/stat's start_time
// (clock ticks since boot, monotonic, never reused for a given PID
// instance) is the canonical defence: a successful match proves "this
// is the same process I scanned earlier", a mismatch proves "PID was
// reused". expectedStartTime==0 means the caller never captured a
// start_time (legacy paths) and we skip the check; the new takeover
// path always populates it from discovery.DiscoveredSession.
func verifyProcIdentity(pid int, expectedStartTime uint64) bool {
	actual, err := discovery.ProcStartTime(pid)
	if err != nil {
		return false
	}
	return actual == expectedStartTime
}

// killAndCleanupClaude terminates an external Claude CLI process and removes its
// stale session/lock files so the session can be cleanly resumed with --resume.
//
// Sequence:
//
//  1. TOCTOU re-check: ProcStartTime must still match the value
//     captured at discovery time. Mismatch => the PID was reused
//     between scan and takeover; abort without signalling.
//  2. SIGTERM: graceful shutdown request. The Claude CLI installs a
//     SIGTERM handler that flushes the JSONL session file and exits
//     cleanly, leaving the on-disk transcript intact for --resume.
//  3. discovery.WaitAndCleanup: polls for exit up to 5 s (its own
//     internal cap; see internal/discovery/scanner.go:1057), then
//     SIGKILLs if the process is still alive AND identity still
//     matches. Also removes ~/.claude/sessions/<pid>.json and the
//     /tmp/claude-<uid>/<cwd>/<sid> lock dir so subsequent --resume
//     does not see "session in use".
//
// The 5 s ceiling on WaitAndCleanup is a tradeoff: long enough for
// the CLI to flush a multi-MB transcript on a slow disk, short enough
// to keep the user's first-message latency tolerable (the takeover
// is on the critical path for the first IM reply). Callers should
// pass a ctx with a tighter deadline if they have stricter budgets.
//
// errors.Is(err, syscall.ESRCH) on SendTerm is benign — it means the
// process exited between identity check and signal, which is exactly
// the outcome we wanted; cleanup proceeds normally.
func (s *Server) killAndCleanupClaude(ctx context.Context, pid int, procStartTime uint64, cwd, sessionID string) error {
	// TOCTOU guard: reject if the PID was recycled since the discovery scan.
	if procStartTime != 0 && !verifyProcIdentity(pid, procStartTime) {
		return fmt.Errorf("process identity changed (PID reused): pid=%d", pid)
	}
	if err := osutil.SendTerm(pid); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("sigterm pid %d: %w", pid, err)
	}
	discovery.WaitAndCleanup(ctx, pid, procStartTime, s.claudeDir, cwd, sessionID)
	return nil
}

// tryAutoTakeover looks for an external Claude CLI session whose CWD matches the
// chat's effective workspace and transparently adopts it under naozhi management.
// Must be called inside the sessionGuard critical section (after TryAcquire).
// Returns true when a session was successfully taken over.
//
// Selection policy: among all discovered sessions whose CWD equals the
// chat's effective workspace, pick the one with the most recent
// LastActive timestamp. This matches operator intuition ("the session
// I was just typing in") even if the user has historically opened
// multiple terminals against the same repo. Stale sessions still
// living in older copies of the workspace are deliberately left
// untouched because they are not selected.
//
// Returns false (without surfacing an error) for every short-circuit
// path — discovery disabled, no candidate, kill failed, resume
// failed. The IM caller transparently falls back to spawning a fresh
// session via the normal GetOrCreate path so the user's message
// always lands somewhere; takeover is a UX optimisation, never a
// correctness gate.
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
	best := mostRecentSessionForCWD(discovered, workspace)
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

// mostRecentSessionForCWD returns the discovered session whose CWD matches
// workspace and whose LastActive is the largest, or nil when none match.
// Pure helper — extracted so tests can pin the LastActive selection
// invariant ("most recent wins, ties resolve to first-seen") without
// running the full takeover pipeline. Returns a pointer into the input
// slice; callers MUST NOT mutate the returned struct without first
// copying.
//
// Returns nil for an empty slice or a workspace that no candidate
// matches; callers treat nil as "no takeover candidate" and fall
// through to the GetOrCreate path.
func mostRecentSessionForCWD(discovered []discovery.DiscoveredSession, workspace string) *discovery.DiscoveredSession {
	var best *discovery.DiscoveredSession
	for i := range discovered {
		ds := &discovered[i]
		if ds.CWD != workspace {
			continue
		}
		if best == nil || ds.LastActive > best.LastActive {
			best = ds
		}
	}
	return best
}
