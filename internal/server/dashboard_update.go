// dashboard_update.go — self-update state for the dashboard.
//
//	GET /api/system/update   version state + what the operator can do
//
// Gated by the same auth middleware as the rest of /api/*. Structured after
// dashboard_system.go: a thin handler over state another subsystem owns.
//
// The endpoint's job is to answer one question — "is there anything to do about
// the version, and what exactly?" — and to answer it in a form the browser can
// use without re-deriving anything. In particular the browser must NOT compare
// versions itself: `action` is computed here (see selfupdate.StatusSnapshot.Action)
// because the distinction between "download it" and "just restart" is subtle
// enough that a second implementation would drift, and the failure mode of
// getting it wrong is destroying the rollback backup (RFC §1.3).
//
// See docs/rfc/dashboard-update-notice.md. P1 is read-only; POST .../apply
// arrives in P2.
package server

import (
	"net/http"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/selfupdate"
)

// updateStatusResponse is the wire shape of GET /api/system/update.
//
// Field-for-field stable: a shape test locks this down, because the dashboard
// branches on `action` and silently mis-rendering the version banner is the
// kind of bug nobody files.
type updateStatusResponse struct {
	// Current is the running build's version ("dev" for local builds).
	Current string `json:"current"`
	// Latest is the newest release tag known to this process, or "" when no
	// check has succeeded yet.
	Latest string `json:"latest"`
	// Staged is a version already written to disk and waiting for a restart.
	Staged string `json:"staged"`
	// Phase is the update subsystem's lifecycle state (see selfupdate.Phase).
	Phase string `json:"phase"`
	// Action is the ONLY field the dashboard should branch on:
	// "none" | "install" | "restart".
	Action string `json:"action"`
	// CheckedAt is when the last release check completed; omitted when none has.
	CheckedAt string `json:"checked_at,omitempty"`
	// CheckError is the last check failure, sanitized. Empty when healthy.
	CheckError string `json:"check_error,omitempty"`
	// LastError is the last install/restart failure, sanitized.
	LastError string `json:"last_error,omitempty"`
	// CanApply reports whether this deployment could actually carry out
	// Action. Advisory — see selfupdate.CheckPreflight.
	CanApply bool `json:"can_apply"`
	// BlockedReason explains CanApply=false in operator-facing Chinese.
	BlockedReason string `json:"blocked_reason,omitempty"`
	// RestartSupported reports whether a managed service (systemd/launchd) was
	// detected that naozhi can restart on its own.
	RestartSupported bool `json:"restart_supported"`
	// RunningSessions lets the UI state the blast radius of a restart.
	RunningSessions int `json:"running_sessions"`
	// Enabled is false when the auto-update checker is not running, in which
	// case Latest is not maintained and the UI should not claim to be current.
	Enabled bool `json:"enabled"`
}

// handleUpdateStatus serves the self-update state.
//
// Always 200 with a complete object, even when the checker is disabled or the
// version is unknown. The dashboard polls this on a timer and a non-200 would
// force it to distinguish "endpoint missing" from "nothing to report" — the
// `enabled` and `action` fields carry that instead.
func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	// Cold-start fill: the default cadence is 6h with check_on_start off, so a
	// recently restarted naozhi has no idea what the latest release is. Without
	// this the dashboard would show nothing for hours — exactly when an
	// operator who just restarted is most likely to be checking.
	//
	// Deliberately narrow: only when we have never learned a tag. CheckNow
	// throttles globally on top of that, so N polling tabs cannot amplify into
	// N requests against GitHub, and a steadily-running deployment never
	// reaches this path at all.
	if s.updateChecker != nil {
		if at, latest := s.updateStatus.LastCheck(); latest == "" && at.IsZero() {
			// Errors are intentionally ignored: this is a best-effort fill and
			// CheckNow has already recorded any failure into Status, which is
			// what we are about to serve.
			_ = s.updateChecker.CheckNow(r.Context())
		}
	}

	snap := s.updateStatus.Snapshot()
	// The running version is authoritative from the build ldflag even when no
	// Status exists (checker disabled), so the "关于" line always has a value.
	current := snap.Current
	if current == "" {
		current = s.buildVersion
	}
	snap.Current = current

	action := snap.Action()
	pre := selfupdate.CheckPreflight(action, current)

	resp := updateStatusResponse{
		Current:          current,
		Latest:           snap.Latest,
		Staged:           snap.Staged,
		Phase:            string(phaseOrIdle(snap.Phase)),
		Action:           string(action),
		CheckError:       snap.CheckErr,
		LastError:        snap.LastErr,
		CanApply:         pre.CanApply,
		BlockedReason:    pre.Reason,
		RestartSupported: selfupdate.ServiceRunning(),
		RunningSessions:  s.runningSessionCount(),
		Enabled:          s.updateStatus != nil,
	}
	if !snap.CheckedAt.IsZero() {
		resp.CheckedAt = snap.CheckedAt.Format(time.RFC3339)
	}
	writeJSON(w, resp)
}

// phaseOrIdle keeps the wire contract non-empty: a nil Status yields the zero
// Phase, and the dashboard should treat that as "idle" rather than having to
// handle an empty string.
func phaseOrIdle(p selfupdate.Phase) selfupdate.Phase {
	if p == "" {
		return selfupdate.PhaseIdle
	}
	return p
}

// runningSessionCount reports how many sessions are mid-turn, so the restart
// confirmation can state the blast radius concretely instead of warning in the
// abstract. Best-effort: a nil router yields 0.
//
// Compares against cli.StateRunning.String() rather than a "running" literal
// so the two stay coupled. Note that StateSpawning also stringifies to
// "running" (it is a transient state shown as running), which is the intended
// semantics here — a spawning session is just as much in-flight work.
func (s *Server) runningSessionCount() int {
	if s.router == nil {
		return 0
	}
	running := cli.StateRunning.String()
	n := 0
	for _, snap := range s.router.ListSessions() {
		if snap.State == running {
			n++
		}
	}
	return n
}
