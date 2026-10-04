package cli

// exit_classify.go — the error a send gets when the CLI exited non-zero, and
// what the CLI's stderr says caused it.

import (
	"strings"

	"github.com/naozhi/naozhi/internal/cli/clierr"
)

// classifyStderr reads what a CLI's stderr tail says made it exit. Matching
// is on substrings of the CLI's English messages, case-insensitive; a pair of
// terms must share a line, and a class earlier in the switch wins when lines
// match several.
func classifyStderr(tail []string) clierr.ExitClass {
	lines := make([]string, len(tail))
	for i, l := range tail {
		lines[i] = strings.ToLower(l)
	}
	switch {
	case anyLine(lines, "", "no conversation found"):
		return clierr.ExitResumeNotFound
	case anyLine(lines, "mcp", "config", "invalid", "failed"):
		return clierr.ExitMCPConfig
	case anyLine(lines, "settings", "invalid", "parse", "not found"):
		return clierr.ExitInvalidSettings
	case anyLine(lines, "", "invalid api key", "please run /login", "authentication", "oauth token has expired", "401 unauthorized"):
		return clierr.ExitAuth
	case anyLine(lines, "", "enoent", "no such file or directory", "command not found", "cannot find module"):
		return clierr.ExitMissingRuntime
	}
	return clierr.ExitUnknown
}

// anyLine reports whether a line contains need ("" for any line) and one of oneOf.
func anyLine(lines []string, need string, oneOf ...string) bool {
	for _, l := range lines {
		if !strings.Contains(l, need) {
			continue
		}
		for _, s := range oneOf {
			if strings.Contains(l, s) {
				return true
			}
		}
	}
	return false
}

// recordExit keeps the error sends get for a CLI that exited with code;
// code 0 keeps the bare ErrProcessExited. A CLI that already wrote stdout
// died mid-session, where its stderr is not a startup cause, so it stays
// ExitUnknown.
func (p *Process) recordExit(code int64, tail []string) {
	if code == 0 {
		return
	}
	e := &clierr.ProcessExitedError{Code: code}
	if !p.sawOutput.Load() {
		e.Class = classifyStderr(tail)
	}
	p.exited.Store(e)
}

// exitErr is the error for a send that found the process dead: the
// *clierr.ProcessExitedError recordExit kept, else clierr.ErrProcessExited.
func (p *Process) exitErr() error {
	if e := p.exited.Load(); e != nil {
		return e
	}
	return clierr.ErrProcessExited
}

// DeathDetail is the stderr line that names why the CLI exited non-zero
// (stderrTailSummary), for the dashboard's exit chip; "" while alive and for
// any other death.
func (p *Process) DeathDetail() string {
	if p.exited.Load() == nil {
		return ""
	}
	return stderrTailSummary(p.StderrTail())
}
