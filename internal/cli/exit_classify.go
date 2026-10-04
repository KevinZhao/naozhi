package cli

// exit_classify.go — the error a send gets when the CLI exited non-zero, and
// what the CLI's stderr says caused it.

import (
	"strconv"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cliinfo"
)

// classifyStderr reads what a CLI's stderr tail says made it exit. Matching
// is on substrings of the CLI's English messages, case-insensitive; a pair of
// terms must share a line, and a class earlier in the switch wins when lines
// match several. Warning lines are skipped: the CLI prints them and runs on
// (a broken --settings file is one), so they name no exit cause.
func classifyStderr(tail []string) clierr.ExitClass {
	lines := make([]string, 0, len(tail))
	for _, l := range tail {
		if !isStderrWarningLine(l) {
			lines = append(lines, strings.ToLower(l))
		}
	}
	switch {
	case anyLine(lines, "", "no conversation found"):
		return clierr.ExitResumeNotFound
	case anyLine(lines, "mcp", "config", "invalid", "failed"):
		return clierr.ExitMCPConfig
	case anyLine(lines, "", "invalid api key", "please run /login", "authentication", "oauth token has expired", "401 unauthorized", "not logged in", "login required"):
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

// noteOutput marks the CLI past startup on its first stdout event, except
// the error_during_execution result claude writes just before exiting when it
// cannot start (a stale --resume id): that frame is the startup failure.
func (p *Process) noteOutput(ev clievent.Event) {
	if ev.Type != "result" || ev.SubType != "error_during_execution" {
		p.sawOutput.Store(true)
	}
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
	p.exitedAt.Store(time.Now().UnixNano())
	p.exited.Store(e)
}

// StartupFailure reports a CLI that exited with a non-zero status of its own
// before it got past startup: the class its stderr names and when it exited.
// ok is false while it runs, after output or a reattach, and for an exit 0,
// a signal (code -1) or a death naozhi caused first (DeathReason).
func (p *Process) StartupFailure() (class clierr.ExitClass, at time.Time, ok bool) {
	e := p.exited.Load()
	if e == nil || e.Code <= 0 || p.sawOutput.Load() ||
		p.DeathReason() != cliinfo.DeathReasonCLIExitedCodePrefix+strconv.FormatInt(e.Code, 10) {
		return clierr.ExitUnknown, time.Time{}, false
	}
	return e.Class, time.Unix(0, p.exitedAt.Load()), true
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
