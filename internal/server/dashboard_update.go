// dashboard_update.go — self-update state and apply for the dashboard.
//
//	GET  /api/system/update        version state + what the operator can do
//	POST /api/system/update/apply  carry it out (install and/or restart)
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
// See docs/rfc/dashboard-update-notice.md.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/ratelimit"
	"github.com/naozhi/naozhi/internal/selfupdate"
	"golang.org/x/time/rate"
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
	// Enabled is false when no auto-update Checker is wired (update.enabled
	// false), in which case Latest is not maintained and the UI should not
	// claim to be current. Keyed on the Checker, not the Status: main.go always
	// builds a Status so `current` is known, so a Status-based flag would be
	// true on every deployment.
	Enabled bool `json:"enabled"`
	// ManualCommand is what the operator runs by hand when the dashboard
	// cannot apply Action itself: `naozhi upgrade` for install, the platform's
	// service restart for restart (only when a managed service runs this
	// process), "" when there is nothing to paste. Computed here because it
	// depends on the SERVER's OS and launchd label, not the browser's.
	ManualCommand string `json:"manual_command"`
	// InstallEnabled reports whether POST .../apply is permitted
	// (update.dashboard_install). False ⇒ the UI shows the manual command
	// instead of a button; the status above is still accurate.
	InstallEnabled bool `json:"install_enabled"`
	// RollbackHint is a paste-ready command that restores the previous binary,
	// shown in the confirmation BEFORE applying — if the new build fails to
	// boot, the dashboard that would carry this advice is gone. Empty when
	// there is nothing to apply.
	RollbackHint string `json:"rollback_hint,omitempty"`
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
	// Deliberately narrow: only while we have never learned a tag. A failed
	// check advances checkedAt but not `latest`, so gating on `latest` alone
	// (not "never tried") lets a later poll retry after one transient failure
	// at boot — otherwise a single blip would leave the chip blank until the 6h
	// tick. CheckNow throttles globally (minOnDemandInterval, stamped before the
	// network call) on top of that, so N polling tabs — or N retries after a
	// failure — cannot amplify into N requests against GitHub, and a
	// steadily-running deployment never reaches this path at all.
	if s.updateChecker != nil {
		if _, latest := s.updateStatus.LastCheck(); latest == "" {
			// Bounded: this is a GET on a polled endpoint, and CheckNow's own
			// bound is 60s. A GitHub that hangs must not hold the status page
			// hostage for a minute — a fill that misses this window is simply
			// picked up by the next poll.
			ctx, cancel := context.WithTimeout(r.Context(), updateColdStartFillTimeout)
			// Errors are intentionally ignored: this is a best-effort fill and
			// CheckNow has already recorded any failure into Status, which is
			// what we are about to serve.
			_ = s.updateChecker.CheckNow(ctx)
			cancel()
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
	// Probe the service ONCE per request and hand the answer to everything that
	// needs it. On darwin this is a `launchctl list` fork (service.go's
	// verifiedLaunchdLabel), and this endpoint is polled by every open
	// dashboard — the preflight gate, the restart_supported field and
	// RollbackHint used to fork separately for the same unchanging fact.
	//
	// ServiceManagesThisProcess, not ServiceRunning: everything downstream is
	// about restarting THIS process. A host with a systemd unit active and this
	// naozhi started by hand beside it must report restart_supported=false, or
	// the button would restart the system service and leave us untouched.
	serviceRunning := selfupdate.ServiceManagesThisProcess()
	pre := selfupdate.CheckPreflight(action, current, serviceRunning)

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
		RestartSupported: serviceRunning,
		RunningSessions:  s.runningSessionCount(),
		Enabled:          s.updateChecker != nil,
		InstallEnabled:   s.updateInstallEnabled && s.updateChecker != nil,
		ManualCommand:    selfupdate.ManualCommand(action, serviceRunning),
	}
	if action != selfupdate.ActionNone {
		resp.RollbackHint = selfupdate.RollbackHint(serviceRunning)
	}
	if !snap.CheckedAt.IsZero() {
		resp.CheckedAt = snap.CheckedAt.Format(time.RFC3339)
	}
	writeJSON(w, resp)
}

// updateApplyRequest is the POST body.
//
// ConfirmAction is required and must equal the `action` the server currently
// computes. It is not redundant with the URL: it closes a TOCTOU in which the
// operator sees "restart" in the chip, the background checker finishes a
// download in the meantime, and the click then means something the operator
// never agreed to. Disagreement is a 409 and the UI re-reads the state.
type updateApplyRequest struct {
	ConfirmAction string `json:"confirm_action"`
}

// newUpdateApplyLimiter builds the throttle for POST .../apply.
//
// What it is for: NOT preventing concurrent installs — InstallLatest's TryLock
// already makes those impossible. It stops a held-down button from spraying the
// failure path: each rejected attempt otherwise writes log lines and, on the
// install branch, a fresh GitHub request.
//
// Note the key is a constant, so this is a GLOBAL limit, not per user. That is
// deliberate and correct here (installing is a whole-process singleton), but it
// is worth stating because the dashboard token is a single shared secret — a
// per-token limiter would look per-user while behaving exactly like this one.
// The token itself is never used as a key: secrets do not belong in a cache
// keyspace that outlives the request.
func newUpdateApplyLimiter() *ratelimit.Limiter {
	return ratelimit.New(ratelimit.Config{
		Rate:    rate.Every(30 * time.Second),
		Burst:   1,
		MaxKeys: 4,
		TTL:     time.Hour,
	})
}

// updateApplyKey is the single bucket every apply attempt shares.
const updateApplyKey = "system-update-apply"

// updateColdStartFillTimeout bounds the on-demand release check a GET may
// trigger to fill the cold-start window. Short by design: this runs inline in a
// polled read endpoint.
const updateColdStartFillTimeout = 10 * time.Second

// applyBackgroundTimeout bounds the detached work. A download of a ~20MB asset
// over a slow link plus verification is minutes at worst; beyond this something
// is wedged and the context should release it.
const applyBackgroundTimeout = 10 * time.Minute

// handleUpdateApply carries out what handleUpdateStatus reported.
//
// Returns 202 and does the work in a detached goroutine. This is not a stylistic
// choice: the successful path ends with this process being SIGTERMed by the
// service manager, so a synchronous handler would be killed before its response
// reached the browser. The client learns the outcome by polling GET — including
// the good outcome, which looks like "the connection dropped, then `current`
// came back as the new version".
func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	// CSRF is covered by the auth middleware's Origin check on non-safe methods
	// (internal/dashboard/auth/csrf.go); no extra gate needed here.
	if !s.updateInstallEnabled {
		http.Error(w, "dashboard install disabled (update.dashboard_install=false)", http.StatusForbidden)
		return
	}
	// Throttle before the state checks so a stuck retry loop cannot spin on
	// them either. A rejected attempt consuming the window is intended: the
	// point is to bound attempts, not just successes.
	if s.updateApplyLimiter != nil && !s.updateApplyLimiter.Allow(updateApplyKey) {
		http.Error(w, "too many update attempts; wait a moment", http.StatusTooManyRequests)
		return
	}
	if s.updateChecker == nil {
		// No checker ⇒ update.enabled is false. The read-only endpoint still
		// works, but there is no subsystem here to run an install.
		http.Error(w, "auto-update is disabled (update.enabled=false)", http.StatusConflict)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	var req updateApplyRequest
	if err := decodeJSONBody(r, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	snap := s.updateStatus.Snapshot()
	if snap.Current == "" {
		snap.Current = s.buildVersion
	}
	action := snap.Action()
	if action == selfupdate.ActionNone {
		http.Error(w, "nothing to apply", http.StatusConflict)
		return
	}
	if req.ConfirmAction != string(action) {
		// The operator confirmed a different operation than the one that would
		// run now. Make them look again rather than guessing which they meant.
		http.Error(w, fmt.Sprintf("state changed: confirmed %q but current action is %q",
			req.ConfirmAction, action), http.StatusConflict)
		return
	}
	// Restart only if there is something to restart. Passing true with no
	// managed service would strand the UI on "restarting" (InstallLatest
	// degrades it too; deciding here keeps the intent visible at the call site).
	// One probe, shared with the preflight gate below — the same fact, and on
	// darwin each read is a `launchctl list` fork. ServiceManagesThisProcess so
	// the restart we ask for is a restart of US (see handleUpdateStatus).
	restart := selfupdate.ServiceManagesThisProcess()
	if pre := selfupdate.CheckPreflight(action, snap.Current, restart); !pre.CanApply {
		http.Error(w, pre.Reason, http.StatusConflict)
		return
	}

	apply := s.updateApplyFn
	if apply == nil {
		apply = s.updateChecker.InstallLatest
	}
	slog.Info("dashboard update: apply requested",
		"action", action, "current", snap.Current, "latest", snap.Latest,
		"staged", snap.Staged, "restart", restart,
		"running_sessions", s.runningSessionCount())

	// Write AND FLUSH the 202 before the work starts, not after. Ordering alone
	// would not be enough: net/http holds a handler's response in its buffer
	// until the handler returns, while on the restart path the goroutine below
	// can have this process SIGTERMed within milliseconds of starting (there is
	// no download in front of the `launchctl kickstart` / `systemctl restart`).
	// Lose that race and the browser sees a dropped connection, which the
	// dashboard reports as "升级请求失败" — telling the operator it failed at the
	// exact moment it is succeeding.
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"status": "started"})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	go func() {
		// A panic here is not in any request's call stack, so nothing above us
		// recovers it: it would take the whole process down mid-upgrade. Same
		// boundary the other detached server goroutines have (wshub_eventpush,
		// send_owner_loop). Status is moved to failed so the chip does not stay
		// parked on "installing" with nothing to show for it.
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("dashboard update: apply goroutine panicked", "action", action, "panic", rec)
				s.updateStatus.MarkFailed(fmt.Errorf("apply aborted: %v", rec))
			}
		}()
		// context.Background, NOT r.Context(): the request is already finished
		// by the time this runs, and its context would be cancelled — killing
		// the download a moment after starting it.
		ctx, cancel := context.WithTimeout(context.Background(), applyBackgroundTimeout)
		defer cancel()
		if err := apply(ctx, restart); err != nil {
			// Everything the operator needs is already in Status (phase +
			// last_error) and will reach them on the next poll; this log line is
			// for the server-side record.
			level := slog.LevelWarn
			if errors.Is(err, selfupdate.ErrNothingToDo) || errors.Is(err, selfupdate.ErrInstallInProgress) {
				level = slog.LevelInfo
			}
			slog.Log(ctx, level, "dashboard update: apply did not complete", "action", action, "err", err)
			return
		}
		slog.Info("dashboard update: apply finished", "action", action)
	}()
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
