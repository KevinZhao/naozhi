package session

// startup_failure.go — what a respawn does after a CLI that failed at
// startup: drop the resume id the failure may be about, and stop respawning a
// key whose fresh processes keep failing too.

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clierr"
)

// ErrCLIStartupFailed is returned by GetOrCreate without spawning while a
// key's CLI keeps exiting at startup; /new clears it at once.
var ErrCLIStartupFailed = errors.New("CLI keeps failing at startup; respawn paused")

// startupFailureReporter is the optional facet of a process that can tell a
// CLI that exited non-zero at startup apart from any other death.
type startupFailureReporter interface {
	StartupFailure() (class clierr.ExitClass, at time.Time, ok bool)
}

var _ startupFailureReporter = (*cli.Process)(nil)

// Startup-failure cooldown: the respawn after the second failure in a row
// waits startupCooldownBase, each further failure doubles it, up to
// startupCooldownMax.
const (
	startupCooldownBase = 30 * time.Second
	startupCooldownMax  = 10 * time.Minute
)

// startupFailure is how s's process failed at startup. streak counts it and
// the failures in a row before it; 0 when s's process got past startup.
type startupFailure struct {
	streak int32
	class  clierr.ExitClass
	at     time.Time
	detail string // the stderr line naming the cause, for logs
}

func startupFailureOf(s *ManagedSession) startupFailure {
	if s == nil {
		return startupFailure{}
	}
	proc := s.loadProcess()
	p, ok := proc.(startupFailureReporter)
	if !ok {
		return startupFailure{}
	}
	class, at, failed := p.StartupFailure()
	if !failed {
		return startupFailure{}
	}
	return startupFailure{streak: s.startupFails.Load() + 1, class: class, at: at, detail: proc.DeathDetail()}
}

// dropsResume reports whether the failure may be the resumed session's own:
// a stale id or a cause stderr does not name. Auth, MCP config and a missing
// runtime fail a fresh spawn the same way, so the resume is kept for the
// retry that follows the operator's fix.
func (f startupFailure) dropsResume() bool {
	return f.streak > 0 && (f.class == clierr.ExitResumeNotFound || f.class == clierr.ExitUnknown)
}

// cooldownLeft is how long a respawn must still wait at now; 0 when it may run.
func (f startupFailure) cooldownLeft(now time.Time) time.Duration {
	if f.streak < 2 {
		return 0
	}
	wait := startupCooldownBase
	for n := f.streak; n > 2 && wait < startupCooldownMax; n-- {
		wait *= 2
	}
	wait = min(wait, startupCooldownMax)
	return max(f.at.Add(wait).Sub(now), 0)
}

// startupBreaker is GetOrCreate's verdict on respawning dead session s: an
// ErrCLIStartupFailed while its cooldown runs, else nil.
func startupBreaker(key string, s *ManagedSession, now time.Time) error {
	f := startupFailureOf(s)
	left := f.cooldownLeft(now).Round(time.Second)
	if left <= 0 {
		return nil
	}
	slog.Warn("CLI keeps failing at startup; respawn paused",
		"key", key, "failures", f.streak, "retry_in", left, "stderr", f.detail)
	return fmt.Errorf("%w (%d in a row, retry in %s)", ErrCLIStartupFailed, f.streak, left)
}

// resumeDropReason is why a respawn of old must not resume its session id,
// with the stderr line behind it: the backend rejected the id at the last
// spawn, or old's CLI failed at startup on it (dropsResume). reason is ""
// when old may resume.
func resumeDropReason(old *ManagedSession) (reason, detail string) {
	if old == nil {
		return "", ""
	}
	if old.resumeRejected.Load() {
		return "backend rejected the resume", ""
	}
	if f := startupFailureOf(old); f.dropsResume() {
		return "CLI failed at startup", f.detail
	}
	return "", ""
}
