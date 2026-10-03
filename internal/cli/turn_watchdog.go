package cli

import (
	"fmt"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clierr"
)

// watchdogMinCheckInterval is the floor of watchdogCheckInterval. A var so
// tests can run millisecond-scale timeouts.
var watchdogMinCheckInterval = time.Second

// monoBase anchors Process.lastOutputNS: storing an offset from it instead of
// UnixNano keeps the monotonic reading, so a wall-clock jump cannot fake or
// hide a no-output stall.
var monoBase = time.Now()

// markOutput records that the CLI produced a frame now.
func (p *Process) markOutput(now time.Time) {
	p.lastOutputNS.Store(int64(now.Sub(monoBase)))
}

// lastOutputAt is the time markOutput last recorded.
func (p *Process) lastOutputAt() time.Time {
	return monoBase.Add(time.Duration(p.lastOutputNS.Load()))
}

// watchdogCheckInterval is how often a waiter re-checks the turn deadlines:
// a quarter of the no-output budget, clamped to [watchdogMinCheckInterval,
// 30s]. The interval caps timeout precision, fine for minute-scale budgets.
func watchdogCheckInterval(noOutputDur time.Duration) time.Duration {
	iv := noOutputDur / 4
	if iv < watchdogMinCheckInterval {
		iv = watchdogMinCheckInterval
	}
	if iv > 30*time.Second {
		iv = 30 * time.Second
	}
	return iv
}

// turnDeadlineVerdict reports which turn budget has run out at now, as the
// death reason and the classified error a waiter returns; ("", nil) when
// neither has. No-output wins when both have.
func turnDeadlineVerdict(now, turnStart, lastOutput time.Time, noOutputDur, totalDur time.Duration) (string, error) {
	if now.Sub(lastOutput) >= noOutputDur {
		return DeathReasonNoOutputTimeout, fmt.Errorf("%w (%s)", clierr.ErrNoOutputTimeout, noOutputDur)
	}
	if now.Sub(turnStart) >= totalDur {
		return DeathReasonTotalTimeout, fmt.Errorf("%w (%s)", clierr.ErrTotalTimeout, totalDur)
	}
	return "", nil
}

// turnBudgets returns the configured no-output and total budgets, defaulted.
func (p *Process) turnBudgets() (noOutputDur, totalDur time.Duration) {
	noOutputDur = p.noOutputTimeout
	if noOutputDur <= 0 {
		noOutputDur = DefaultNoOutputTimeout
	}
	totalDur = p.totalTimeout
	if totalDur <= 0 {
		totalDur = DefaultTotalTimeout
	}
	return noOutputDur, totalDur
}

// passthroughWatchdogTick checks the passthrough turn the CLI owes the queue
// against both budgets and kills the process when one has run out. Any queued
// slot counts, a canceled tombstone included: the CLI still owes its result.
// Returns the classified error, or nil when the turn is within budget.
func (p *Process) passthroughWatchdogTick(now time.Time, noOutputDur, totalDur time.Duration) error {
	p.slots.mu.Lock()
	turnStart := p.slots.turnStartedAt
	queued := len(p.slots.pending) > 0
	p.slots.mu.Unlock()
	if !queued || turnStart.IsZero() {
		return nil
	}
	reason, err := turnDeadlineVerdict(now, turnStart, p.lastOutputAt(), noOutputDur, totalDur)
	if err != nil {
		p.logWatchdogKill(reason, noOutputDur, totalDur, "passthrough")
		p.watchdogKillPassthrough(reason, err)
	}
	return err
}

// logWatchdogKill logs a watchdog kill under the message for its reason.
func (p *Process) logWatchdogKill(reason string, noOutputDur, totalDur time.Duration, mode string) {
	if reason == DeathReasonNoOutputTimeout {
		p.slogger().Error("watchdog: no output timeout", "timeout", noOutputDur, "mode", mode)
		return
	}
	p.slogger().Error("watchdog: total timeout", "timeout", totalDur, "mode", mode)
}

// watchdogKillPassthrough kills a stalled passthrough CLI. The death reason
// goes first so readLoop's EOF classification cannot overwrite it, and every
// queued caller gets err before Kill lets readLoop hand them ErrProcessExited.
// Every step is idempotent; a waiter ticking after another's kill finds the
// queue already discarded and never gets here.
func (p *Process) watchdogKillPassthrough(reason string, err error) {
	p.setDeathReason(reason)
	p.discardAllPending(err)
	p.Kill()
	p.clearInflightFlags()
}
