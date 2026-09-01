// status.go — the shared version-state object between the background Checker
// and the dashboard.
//
// Why this exists: the Checker already polls GitHub Releases on
// cfg.UpdateInterval() (6h default). The dashboard must NOT poll GitHub
// itself — every connected browser would multiply the request rate against a
// third party for information that is identical for all of them. So the
// Checker writes what it learned here and the dashboard reads it.
//
// The single most important thing this file encodes is the distinction
// between "a newer release exists" and "a newer binary is already on disk,
// waiting for a restart". Those look similar but need OPPOSITE actions, and
// conflating them corrupts the rollback artifact — see ActionRestart and
// docs/rfc/dashboard-update-notice.md §1.2/§1.3.
package selfupdate

import (
	"sync"
	"time"
)

// Phase is the lifecycle state of the update subsystem in this process.
type Phase string

const (
	// PhaseIdle means there is nothing to do: either no check has succeeded
	// yet, or the latest release is not newer than what we run.
	PhaseIdle Phase = "idle"

	// PhaseAvailable means a newer release exists and the on-disk binary has
	// NOT been replaced yet.
	//
	// Under the default mode ("download") this state is nearly unobservable:
	// the Checker replaces the binary within seconds of finding it (measured:
	// 3s on this project's own deployment). It is the steady state only for
	// mode "notify", or in the window before a download completes.
	PhaseAvailable Phase = "available"

	// PhaseInstalling means a download/verify/replace cycle is in flight.
	PhaseInstalling Phase = "installing"

	// PhaseStaged means the new binary IS on disk but this process still runs
	// the old one — it applies on the next restart.
	//
	// This is the steady state operators actually see under the default
	// "download" mode, and the reason this whole feature exists: nothing in
	// the product surfaced it, so deployments sat on a staged binary for
	// hours (22h observed) with no signal that a restart was all it needed.
	PhaseStaged Phase = "staged"

	// PhaseRestarting means a restart has been triggered and this process is
	// about to be killed. It is the last state this generation reports.
	PhaseRestarting Phase = "restarting"

	// PhaseFailed means the most recent install/restart attempt failed;
	// LastErr carries the (sanitized) reason. The service keeps running the
	// old binary — Replace restores its backup on any swap failure.
	PhaseFailed Phase = "failed"
)

// Action is the single field the dashboard is allowed to branch on. The
// server computes it so the browser never re-implements semver comparison or
// the phase state machine — two copies of that logic would drift.
type Action string

const (
	// ActionNone: nothing for the operator to do.
	ActionNone Action = "none"

	// ActionInstall: a newer release exists and is not yet on disk. Applying
	// means download → verify → replace → restart.
	ActionInstall Action = "install"

	// ActionRestart: the new binary is already staged on disk. Applying means
	// RESTART ONLY.
	//
	// It must never mean "install again". Replace() backs up whatever is
	// currently at installPath, so a second Replace in the staged state would
	// copy the NEW binary over the backup (O_TRUNC), destroying the only copy
	// of the version we could roll back to. See RFC §1.3.
	ActionRestart Action = "restart"
)

// StatusSnapshot is an immutable value copy handed to the HTTP layer. Callers
// may hold it as long as they like; it never aliases Status' guarded fields.
type StatusSnapshot struct {
	Current   string
	Latest    string
	Staged    string
	Phase     Phase
	CheckedAt time.Time
	CheckErr  string
	LastErr   string
}

// Action derives what the operator can do from the snapshot alone, so the
// HTTP handler and the tests agree on one implementation.
//
// Order matters: the staged check comes FIRST. In the staged state Latest is
// also greater than Current (that is why it was staged), so a naive
// "Latest > Current ⇒ install" test would report install and walk straight
// into the backup-destroying path described on ActionRestart.
func (s StatusSnapshot) Action() Action {
	// Staged binary that differs from what we run ⇒ a restart is all it takes.
	if s.Staged != "" && s.Staged != s.Current {
		return ActionRestart
	}
	// Otherwise only a strictly-newer remote tag is actionable. semverGreater
	// returns false on unparseable input, which conservatively yields
	// ActionNone — and it is also what blocks a downgrade from being offered
	// as an "update" (R20260602141221-SEC-1).
	if s.Latest != "" && semverGreater(s.Latest, s.Current) {
		return ActionInstall
	}
	return ActionNone
}

// Status is the mutable shared state. Writers are the Checker (background
// tick) and — from P2 on — the dashboard apply handler; the reader is the
// HTTP GET. Every field is guarded by mu.
//
// A nil *Status is a valid no-op receiver on every method. That is load
// bearing: it lets the Checker run exactly as it did before this file
// existed, which is what keeps the pre-existing checker tests meaningful as
// a regression gate rather than something we had to edit to stay green.
type Status struct {
	mu        sync.Mutex
	current   string
	latest    string
	staged    string
	phase     Phase
	checkedAt time.Time
	checkErr  string
	lastErr   string
}

// NewStatus builds a Status for a process running version `current`.
func NewStatus(current string) *Status {
	return &Status{current: current, phase: PhaseIdle}
}

// StatusFixture describes a Status to construct directly, for tests in OTHER
// packages (the HTTP handler's, primarily) that need to exercise a specific
// version state.
//
// The real mutators stay unexported on purpose: only the Checker writes this
// object in production, and keeping noteCheck/notePhase package-private is what
// prevents a future handler from "helpfully" mutating shared state on a GET.
// This is the one seam that opens for tests.
type StatusFixture struct {
	Current  string
	Latest   string
	Staged   string
	Phase    Phase
	CheckErr string
	LastErr  string
}

// NewStatusFixture builds a Status in an arbitrary state. Test-only.
func NewStatusFixture(f StatusFixture) *Status {
	phase := f.Phase
	if phase == "" {
		phase = PhaseIdle
	}
	return &Status{
		current:   f.Current,
		latest:    f.Latest,
		staged:    f.Staged,
		phase:     phase,
		checkErr:  f.CheckErr,
		lastErr:   f.LastErr,
		checkedAt: time.Now(),
	}
}

// Snapshot returns a value copy for the HTTP layer.
func (s *Status) Snapshot() StatusSnapshot {
	if s == nil {
		return StatusSnapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return StatusSnapshot{
		Current:   s.current,
		Latest:    s.latest,
		Staged:    s.staged,
		Phase:     s.phase,
		CheckedAt: s.checkedAt,
		CheckErr:  s.checkErr,
		LastErr:   s.lastErr,
	}
}

// Current reports the running version without taking a full snapshot.
func (s *Status) Current() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

// LastCheck reports when the last check completed and whether a tag was
// learned. The GET handler uses this to decide whether an on-demand check is
// warranted (cold-start window — see Checker.CheckNow).
func (s *Status) LastCheck() (at time.Time, latest string) {
	if s == nil {
		return time.Time{}, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkedAt, s.latest
}

// noteCheck records the outcome of a release check.
//
// checkedAt advances even on failure: it marks "we tried", which is what the
// on-demand throttle needs in order to avoid hammering GitHub while it is
// unreachable.
//
// A failed check deliberately leaves `latest` untouched rather than clearing
// it. A transient network blip should not make a genuine pending update
// vanish from the dashboard.
func (s *Status) noteCheck(latest string, err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkedAt = time.Now()
	if err != nil {
		s.checkErr = sanitizeErr(err)
		return
	}
	s.checkErr = ""
	s.latest = latest
	// Only advance the phase when it carries no more specific information.
	// Overwriting PhaseStaged/PhaseInstalling/PhaseFailed here would erase
	// state the operator still needs to see.
	if s.phase == PhaseIdle || s.phase == PhaseAvailable {
		if semverGreater(latest, s.current) {
			s.phase = PhaseAvailable
		} else {
			s.phase = PhaseIdle
		}
	}
}

// notePhase records a phase transition. `staged` is applied only when
// non-empty, so callers reporting an unrelated transition cannot
// accidentally clear a real staged tag.
func (s *Status) notePhase(p Phase, staged string, err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phase = p
	if staged != "" {
		s.staged = staged
	}
	if err != nil {
		s.lastErr = sanitizeErr(err)
		return
	}
	// A successful transition clears the previous failure so the dashboard
	// does not keep showing a stale error next to a healthy state.
	s.lastErr = ""
}
