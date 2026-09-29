package session

import (
	"errors"
	"log/slog"
	"os"
	"strings"
	"syscall"
)

// Stateless resume-target validation + Claude-CLI project-directory helpers.
// These touch no Router state.

// isENOENTErr reports whether err (or anything it wraps) carries
// syscall.ENOENT. Do NOT match the strerror text instead: it is
// locale-dependent (LANG=zh_CN.UTF-8 yields a Chinese translation).
func isENOENTErr(err error) bool {
	return err != nil && errors.Is(err, syscall.ENOENT)
}

// resolveResumeID returns "" — a fresh session — when resumeID's on-disk state
// is gone, so a missing claude jsonl (work_dir changed, or the prior process
// died before flushing a turn) cannot loop every tick on the CLI's exit 1 "No
// conversation found". Where the state lives is the backend's layout
// (backend.Profile.ResumeTarget, dir = backendDirs[backendID]); a backend
// without one (codex) or an unregistered ID is not pre-checked. A stat error
// other than ErrNotExist downgrades too. Empty backendID is a legacy claude
// session. A resumeID with a path separator or ".." is rejected outright: it
// is joined against a trusted root.
func resolveResumeID(backendID, claudeDir string, backendDirs map[string]string, workspace, key, resumeID string) string {
	if resumeID == "" {
		return resumeID
	}
	if strings.ContainsAny(resumeID, `/\`) || strings.Contains(resumeID, "..") {
		slog.Warn("resume id malformed, starting fresh session",
			"key", key, "resume_id_len", len(resumeID))
		return ""
	}
	if backendID == "" {
		backendID = "claude"
	}
	p, ok := backendProfile(backendID)
	if !ok || p.ResumeTarget == nil {
		return resumeID
	}
	target := p.ResumeTarget(backendDirs[backendID], claudeDir, workspace, resumeID)
	if target == "" {
		return resumeID
	}
	if _, err := os.Stat(target); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Warn("resume target missing, starting fresh session",
				"key", key, "resume_id", resumeID, "backend", backendID,
				"workspace", workspace, "expected_path", target)
		} else {
			slog.Warn("resume target stat failed, starting fresh session",
				"key", key, "resume_id", resumeID, "backend", backendID,
				"expected_path", target, "err", err)
		}
		return ""
	}
	return resumeID
}
