package session

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/naozhi/naozhi/internal/discovery"
)

// File: claudeproject.go
//
// Stateless resume-target validation + Claude-CLI project-directory helpers
// relocated from router_core.go (R20260607-ARCH-4 / #1907). These touch no
// Router state — they map a CWD to Claude's ~/.claude/projects/ layout and
// validate a resume target on disk for whichever backend will consume it.

// isENOENTErr reports whether err (or any error it wraps) ultimately
// carries syscall.ENOENT. The helper exists primarily to make the intent
// explicit at call sites and to spell out why we must NOT match the
// strerror text ("no such file or directory") — it is locale-dependent
// (e.g. LANG=zh_CN.UTF-8 returns a Chinese translation) and silently
// regresses under non-English containers. errors.Is already walks the
// %w chain through *os.PathError / *os.SyscallError transparently, so
// a single call suffices.
func isENOENTErr(err error) bool {
	return err != nil && errors.Is(err, syscall.ENOENT)
}

// claudeProjectSlug maps a CWD to the directory name Claude CLI uses under
// ~/.claude/projects/. Thin wrapper over discovery.ClaudeProjectSlug so the
// two call sites (session + discovery) can never drift: if Claude's naming
// scheme ever changes, the single implementation in internal/discovery is
// the one to edit. TestClaudeProjectSlug_MatchesDiscovery pins the behaviour.
// RNEW-002.
func claudeProjectSlug(cwd string) string {
	return discovery.ClaudeProjectSlug(cwd)
}

// resolveResumeID validates that resumeID's on-disk session state still
// exists for the backend that will consume it, returning "" to downgrade the
// spawn to a fresh session when it does not.
//
// Backend dispatch (incident 2026-07-14, kiro "blank start"): the pre-check
// was previously claude-only in shape but ran unconditionally for every
// backend, so a kiro resume — whose state lives at
// ~/.kiro/sessions/cli/<sid>.json, NOT under ~/.claude/projects/<slug>/ —
// ENOENT'd 100% of the time and silently downgraded to a fresh session,
// losing the whole conversation context even though session/load would have
// restored it (multi-backend-validation.md V2). Each backend now probes its
// own layout:
//
//   - "claude" / "" (legacy default): <claudeDir>/projects/<slug(workspace)>/<id>.jsonl
//     — what `claude --resume` reads (workspace-slug-keyed).
//   - "kiro": <kiroSessionsDir>/<id>.json — kiro's UUID-keyed session state
//     consumed by ACP `session/load` (workspace-independent).
//   - anything else (codex thread/resume, future backends): no pre-check.
//     codex rollouts are date-bucketed (~/.codex/sessions/YYYY/MM/DD/…) with
//     no cheap existence probe; a missing target surfaces as a protocol
//     Init error instead of a silent context loss.
//
// A resumeID containing path separators or ".." is rejected outright (all
// branches): it flows into filepath.Join against a trusted root, and while
// every current producer is UUID-shaped, defense-in-depth here is one line.
func resolveResumeID(backendID, claudeDir, kiroSessionsDir, workspace, key, resumeID string) string {
	if resumeID == "" {
		return resumeID
	}
	if strings.ContainsAny(resumeID, `/\`) || strings.Contains(resumeID, "..") {
		slog.Warn("resume id malformed, starting fresh session",
			"key", key, "resume_id_len", len(resumeID))
		return ""
	}
	switch backendID {
	case "kiro":
		return resolveKiroResumeID(kiroSessionsDir, key, resumeID)
	case "claude", "":
		return resolveClaudeResumeID(claudeDir, workspace, key, resumeID)
	default:
		return resumeID
	}
}

// resolveClaudeResumeID returns resumeID if the corresponding jsonl
// conversation file exists under claudeDir (i.e. Claude CLI's --resume will
// actually find it), or "" to downgrade the spawn to a fresh session.
//
// Motivating failure: a cron job whose work_dir is edited after first run
// stores its jsonl under the original workspace's slug; subsequent ticks
// compute the new slug and --resume hits a path that does not exist, so
// Claude CLI prints "No conversation found with session ID: <id>" to stderr
// and exits 1 in ~1.7s. Upstream sees cron_job completed with result_len=0
// and no recorded error. Same failure mode fires when the prior CLI process
// died before flushing any turn — shim captured the init event's session_id
// but no jsonl was ever produced, so every subsequent tick keeps generating
// fresh-but-unsaved ids in a loop.
//
// Skipped when claudeDir or workspace are empty (test harness / misconfig):
// without both we can't build a meaningful path, and preserving legacy
// behavior keeps unrelated unit tests independent of filesystem layout.
// On stat errors other than ErrNotExist (permission denied, I/O failure)
// we also downgrade — a broken claudeDir would otherwise manifest as the
// same silent exit-1 loop the primary fix targets.
func resolveClaudeResumeID(claudeDir, workspace, key, resumeID string) string {
	if claudeDir == "" || workspace == "" {
		return resumeID
	}
	jsonlPath := filepath.Join(claudeDir, "projects",
		claudeProjectSlug(workspace), resumeID+".jsonl")
	if _, err := os.Stat(jsonlPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Warn("resume target missing, starting fresh session",
				"key", key,
				"resume_id", resumeID,
				"workspace", workspace,
				"expected_path", jsonlPath)
		} else {
			slog.Warn("resume target stat failed, starting fresh session",
				"key", key,
				"resume_id", resumeID,
				"expected_path", jsonlPath,
				"err", err)
		}
		return ""
	}
	return resumeID
}

// resolveKiroResumeID returns resumeID if kiro's session-state file exists
// under kiroSessionsDir (i.e. ACP `session/load` will actually find it), or
// "" to downgrade the spawn to a fresh session.
//
// kiro persists exactly one <sid>.json metadata file per session (plus a
// <sid>.jsonl transcript that may legitimately be empty for a session with
// no completed turn), keyed by the session UUID with no workspace-slug
// component — so unlike the claude probe, workspace never participates.
// A stale .lock does not block resume: kiro performs stale-PID lock
// auto-recovery on load (multi-backend-validation.md V2).
//
// Skipped when kiroSessionsDir is empty (test harness / misconfig), matching
// resolveClaudeResumeID's empty-claudeDir behaviour.
func resolveKiroResumeID(kiroSessionsDir, key, resumeID string) string {
	if kiroSessionsDir == "" {
		return resumeID
	}
	statePath := filepath.Join(kiroSessionsDir, resumeID+".json")
	if _, err := os.Stat(statePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Warn("resume target missing, starting fresh session",
				"key", key,
				"resume_id", resumeID,
				"backend", "kiro",
				"expected_path", statePath)
		} else {
			slog.Warn("resume target stat failed, starting fresh session",
				"key", key,
				"resume_id", resumeID,
				"backend", "kiro",
				"expected_path", statePath,
				"err", err)
		}
		return ""
	}
	return resumeID
}
