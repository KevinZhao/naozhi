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

// Stateless resume-target validation + Claude-CLI project-directory helpers.
// These touch no Router state.

// isENOENTErr reports whether err (or anything it wraps) carries
// syscall.ENOENT. Do NOT match the strerror text instead: it is
// locale-dependent (LANG=zh_CN.UTF-8 yields a Chinese translation).
func isENOENTErr(err error) bool {
	return err != nil && errors.Is(err, syscall.ENOENT)
}

// claudeProjectSlug maps a CWD to the directory name Claude CLI uses under
// ~/.claude/projects/. Delegates to discovery.ClaudeProjectSlug so the two
// call sites cannot drift; TestClaudeProjectSlug_MatchesDiscovery pins it.
func claudeProjectSlug(cwd string) string {
	return discovery.ClaudeProjectSlug(cwd)
}

// resolveResumeID validates that resumeID's on-disk session state still
// exists for the backend that will consume it, returning "" to downgrade the
// spawn to a fresh session when it does not. Each backend probes its own layout:
//   - "claude" / "": <claudeDir>/projects/<slug(workspace)>/<id>.jsonl (what `claude --resume` reads).
//   - "kiro": <kiroSessionsDir>/<id>.json, UUID-keyed and workspace-independent (ACP `session/load`).
//   - anything else: no pre-check; codex rollouts are date-bucketed with no
//     cheap probe, so a missing target surfaces as a protocol Init error.
//
// A resumeID containing path separators or ".." is rejected outright: it
// flows into filepath.Join against a trusted root (defense-in-depth).
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
// Without it a missing jsonl (work_dir changed → different slug, or the prior
// process died before flushing a turn) makes the CLI exit 1 with "No
// conversation found" and every tick loops on fresh-but-unsaved ids.
//
// Skipped when claudeDir or workspace is empty (test harness / misconfig).
// Stat errors other than ErrNotExist also downgrade — a broken claudeDir
// would otherwise produce the same silent exit-1 loop.
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
// "" to downgrade the spawn to a fresh session. kiro keys <sid>.json by
// session UUID with no workspace component, so workspace never participates;
// a stale .lock does not block resume (kiro auto-recovers stale-PID locks).
// Skipped when kiroSessionsDir is empty (test harness / misconfig).
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
