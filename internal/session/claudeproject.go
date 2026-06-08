package session

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"github.com/naozhi/naozhi/internal/discovery"
)

// File: claudeproject.go
//
// Stateless Claude-CLI project-directory + resume-id helpers relocated from
// router_core.go (R20260607-ARCH-4 / #1907). These touch no Router state —
// they map a CWD to Claude's ~/.claude/projects/<slug>/ layout and validate
// a resume target on disk. Pure code-relocation — no behaviour change.

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

// resolveResumeID returns resumeID if the corresponding jsonl conversation
// file exists under claudeDir (i.e. Claude CLI's --resume will actually find
// it), or "" to downgrade the spawn to a fresh session.
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
func resolveResumeID(claudeDir, workspace, key, resumeID string) string {
	if resumeID == "" || claudeDir == "" || workspace == "" {
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
