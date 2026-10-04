package session

// startup_failure.go — what a respawn does after a CLI that failed at
// startup: drop the resume id the failure may be about, and stop respawning a
// key whose fresh processes keep failing too, whether they died after the
// spawn or failed it in the Init handshake.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session/sessionview"
	"github.com/naozhi/naozhi/internal/session/spawnpool"
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

// retryAt is when the cooldown after f ends; zero when f starts none.
func (f startupFailure) retryAt() time.Time {
	if f.streak < 2 {
		return time.Time{}
	}
	wait := startupCooldownBase
	for n := f.streak; n > 2 && wait < startupCooldownMax; n-- {
		wait *= 2
	}
	return f.at.Add(min(wait, startupCooldownMax))
}

// cooldownLeft is how long a respawn must still wait at now; 0 when it may run.
func (f startupFailure) cooldownLeft(now time.Time) time.Duration {
	t := f.retryAt()
	if t.IsZero() {
		return 0
	}
	return max(t.Sub(now), 0)
}

// orRun is the later of f and run, a key's run of failed spawns (ok: the key
// has one).
func (f startupFailure) orRun(run spawnpool.StartupFailure, ok bool) startupFailure {
	if ok && run.At.After(f.at) {
		return startupFailure{streak: run.Streak, at: run.At, detail: run.Detail}
	}
	return f
}

// startupBreaker is GetOrCreate's verdict on spawning key, whose entry is
// dead or absent: an ErrCLIStartupFailed while the cooldown of its latest
// startup failure runs, else nil. The latest is the entry's process or the
// key's run of failed spawns, whichever failed last.
func startupBreaker(tx sessTx, key string, now time.Time) error {
	f := startupFailureOf(tx.Get(key)).orRun(tx.Ext().spawns.StartupFailure(key))
	left := f.cooldownLeft(now).Round(time.Second)
	if left <= 0 {
		return nil
	}
	slog.Warn("CLI keeps failing at startup; respawn paused",
		"key", key, "failures", f.streak, "retry_in", left, "cause", f.detail)
	return fmt.Errorf("%w (%d in a row, retry in %s)", ErrCLIStartupFailed, f.streak, left)
}

// countsAsStartupFailure reports whether a spawn's err says its CLI fails at
// startup: the Init handshake failed while ctx was live. A rejected resume is
// about the session id, and GetOrCreate retries it fresh.
func countsAsStartupFailure(ctx context.Context, err error) bool {
	return errors.Is(err, clierr.ErrSpawnInit) && !errors.Is(err, clierr.ErrResumeRejected) && ctx.Err() == nil
}

// noteSpawnFailure records err as key's latest failed spawn, continuing the
// streak of the key's run or of its dead entry's process.
func noteSpawnFailure(tx sessTx, key string, err error, now time.Time) {
	cur := tx.Get(key)
	if cur != nil && cur.isAlive() {
		return // another path installed a live session: nothing to pause
	}
	rec, _ := tx.Ext().spawns.StartupFailure(key)
	tx.Ext().spawns.NoteStartupFailure(key, spawnpool.StartupFailure{
		Streak: max(rec.Streak, startupFailureOf(cur).streak) + 1,
		At:     now,
		Detail: osutil.SanitizeForLog(err.Error(), 200),
	})
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

// startupFailureView is what the dashboard shows of s's startup failures:
// the later of its process's and run (its key's failed spawns, ok: the key
// has one), and whether the next send drops the resume. nil when s is alive
// or there is neither to show.
func startupFailureView(s *ManagedSession, run spawnpool.StartupFailure, ok bool) *sessionview.StartupFailureView {
	if s.isAlive() {
		return nil
	}
	f := startupFailureOf(s).orRun(run, ok)
	drop, _ := resumeDropReason(s)
	if f.streak == 0 && drop == "" {
		return nil
	}
	v := &sessionview.StartupFailureView{Streak: f.streak, NewSession: drop != ""}
	if f.streak > 0 {
		v.Class = f.class.Wire()
	}
	if t := f.retryAt(); !t.IsZero() {
		v.RetryAt = t.UnixMilli()
	}
	return v
}
