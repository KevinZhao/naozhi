// status.go — version state shared between the Checker (writer) and the
// dashboard (reader), so browsers never poll GitHub themselves. The key
// distinction it encodes: "a newer release exists" vs "a newer binary is
// staged on disk" need OPPOSITE actions, and conflating them corrupts the
// rollback artifact (docs/rfc/dashboard-update-notice.md §1.2/§1.3).
package selfupdate

import (
	"errors"
	"sync"
	"time"
)

// Phase is the lifecycle state of the update subsystem in this process.
type Phase string

const (
	// PhaseIdle: no check succeeded yet, or latest is not newer than current.
	PhaseIdle Phase = "idle"

	// PhaseAvailable: a newer release exists and the binary is NOT replaced
	// yet. Nearly unobservable under mode "download"; steady only for "notify".
	PhaseAvailable Phase = "available"

	// PhaseInstalling means a download/verify/replace cycle is in flight.
	PhaseInstalling Phase = "installing"

	// PhaseStaged: the new binary IS on disk but this process still runs the
	// old one — the steady state under mode "download", applied on restart.
	PhaseStaged Phase = "staged"

	// PhaseRestarting: a restart was triggered; the last state this generation reports.
	PhaseRestarting Phase = "restarting"

	// PhaseFailed: the last install/restart failed (LastErr has the reason);
	// the service keeps running the old binary.
	PhaseFailed Phase = "failed"
)

// Action is the single field the dashboard branches on, computed server-side
// so the browser never re-implements semver or the phase machine.
type Action string

const (
	// ActionNone: nothing for the operator to do.
	ActionNone Action = "none"

	// ActionInstall: newer release, not yet on disk — download → verify → replace → restart.
	ActionInstall Action = "install"

	// ActionRestart: the new binary is staged — RESTART ONLY. It must never
	// mean "install again": a second Replace would copy the NEW binary over
	// the backup and destroy the only rollback copy (RFC §1.3).
	ActionRestart Action = "restart"
)

// StatusSnapshot is an immutable value copy handed to the HTTP layer.
type StatusSnapshot struct {
	Current   string
	Latest    string
	Staged    string
	Phase     Phase
	CheckedAt time.Time
	CheckErr  string
	LastErr   string
}

// Action derives what the operator can do from the snapshot alone. The staged
// check comes FIRST: in the staged state Latest > Current too, and a naive
// "install" verdict walks into the backup-destroying path.
func (s StatusSnapshot) Action() Action {
	if s.Staged != "" && s.Staged != s.Current {
		return ActionRestart
	}
	// Strictly newer only; semverGreater's false on bad input also blocks a
	// downgrade from being offered as an "update".
	if s.Latest != "" && semverGreater(s.Latest, s.Current) {
		return ActionInstall
	}
	return ActionNone
}

// Status is the mutable shared state; every field is guarded by mu. A nil
// *Status is a valid no-op receiver on every method.
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

// StatusFixture builds a Status in an arbitrary state for tests in OTHER
// packages. The real mutators stay unexported so no handler can mutate shared
// state on a GET; this is the one seam that opens for tests.
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

// LastCheck reports when the last check completed and the tag learned; the GET
// handler uses it to decide whether an on-demand check is warranted.
func (s *Status) LastCheck() (at time.Time, latest string) {
	if s == nil {
		return time.Time{}, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkedAt, s.latest
}

// noteCheck records a release check. checkedAt advances even on failure (the
// throttle needs "we tried"); a failed check leaves `latest` untouched so a
// network blip does not hide a genuine pending update.
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
	// Never overwrite a more specific phase (Staged/Installing/Failed).
	if s.phase == PhaseIdle || s.phase == PhaseAvailable {
		if semverGreater(latest, s.current) {
			s.phase = PhaseAvailable
		} else {
			s.phase = PhaseIdle
		}
	}
}

// notePhase records a phase transition; `staged` is applied only when
// non-empty so unrelated transitions cannot clear a real staged tag.
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
	s.lastErr = ""
}

// MarkFailed records an apply that died outside the Checker's own error
// handling (the HTTP layer's recover() around the detached apply goroutine) so
// the phase is not parked on `installing`. It does not touch `staged`: a binary
// written before the panic must still be offered a restart, not a re-install.
func (s *Status) MarkFailed(err error) {
	if err == nil {
		err = errors.New("update apply aborted")
	}
	s.notePhase(PhaseFailed, "", err)
}
