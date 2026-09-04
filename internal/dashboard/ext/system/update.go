package system

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/ratelimit"
	"github.com/naozhi/naozhi/internal/selfupdate"
	"golang.org/x/time/rate"
)

// updateStatusResponse is the wire shape of GET /api/system/update.
// Field-for-field stable; a shape test locks it down.
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
	// Enabled is false when no auto-update Checker is wired, in which case
	// Latest is not maintained. Keyed on the Checker, not the Status (main.go
	// always builds a Status).
	Enabled bool `json:"enabled"`
	// ManualCommand is what the operator runs by hand when the dashboard
	// cannot apply Action itself; "" when there is nothing to paste. Computed
	// here because it depends on the SERVER's OS and service label.
	ManualCommand string `json:"manual_command"`
	// InstallEnabled reports whether POST .../apply is permitted
	// (update.dashboard_install). False ⇒ the UI shows the manual command.
	InstallEnabled bool `json:"install_enabled"`
	// RollbackHint is a paste-ready command that restores the previous binary,
	// shown BEFORE applying (if the new build fails to boot the dashboard is
	// gone). Empty when there is nothing to apply.
	RollbackHint string `json:"rollback_hint,omitempty"`
}

// HandleUpdateStatus serves the self-update state.
//
// Always 200 with a complete object, even when the checker is disabled or the
// version is unknown; `enabled` and `action` carry that instead. The browser
// must NOT compare versions itself: `action` is computed here
// (selfupdate.StatusSnapshot.Action) because getting "download" vs "just
// restart" wrong destroys the rollback backup (docs/rfc/dashboard-update-notice.md).
func (h *Handlers) HandleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	// Cold-start fill while no tag has ever been learned (a failed check
	// advances checkedAt but not `latest`, so a boot-time blip is retried on
	// the next poll). CheckNow throttles globally so N polling tabs cannot
	// amplify into N GitHub requests.
	if h.updateChecker != nil {
		if _, latest := h.updateStatus.LastCheck(); latest == "" {
			// Bounded so a hanging GitHub cannot hold the polled status page
			// hostage; a fill that misses the window is picked up next poll.
			// Errors are ignored: CheckNow already recorded them into Status.
			ctx, cancel := context.WithTimeout(r.Context(), updateColdStartFillTimeout)
			_ = h.updateChecker.CheckNow(ctx)
			cancel()
		}
	}

	snap := h.updateStatus.Snapshot()
	// Fall back to the build ldflag so "关于" always has a value.
	current := snap.Current
	if current == "" {
		current = h.buildVersion
	}
	snap.Current = current

	action := snap.Action()
	// Probe the service ONCE per request (on darwin it is a `launchctl list`
	// fork and every dashboard polls this). ServiceManagesThisProcess, not
	// ServiceRunning: a hand-started naozhi beside an active unit must report
	// restart_supported=false or the button would restart the wrong process.
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
		RunningSessions:  h.runningSessionCount(),
		Enabled:          h.updateChecker != nil,
		InstallEnabled:   h.installEnabled && h.updateChecker != nil,
		ManualCommand:    selfupdate.ManualCommand(action, serviceRunning),
	}
	if action != selfupdate.ActionNone {
		resp.RollbackHint = selfupdate.RollbackHint(serviceRunning)
	}
	if !snap.CheckedAt.IsZero() {
		resp.CheckedAt = snap.CheckedAt.Format(time.RFC3339)
	}
	httputil.WriteJSON(w, resp)
}

// updateApplyRequest is the POST body.
//
// ConfirmAction must equal the `action` the server currently computes; it
// closes the TOCTOU where the checker finishes a download between the
// operator seeing "restart" and clicking. Disagreement is a 409.
type updateApplyRequest struct {
	ConfirmAction string `json:"confirm_action"`
}

// newUpdateApplyLimiter builds the throttle for POST .../apply. It bounds
// attempts (each rejected one logs and may hit GitHub), not concurrency —
// InstallLatest's TryLock handles that. The key is a constant so this is a
// GLOBAL limit, deliberately: installing is a whole-process singleton, and the
// dashboard token must never be used as a cache key.
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
// trigger; it runs inline in a polled read endpoint.
const updateColdStartFillTimeout = 10 * time.Second

// applyBackgroundTimeout bounds the detached download+verify work.
const applyBackgroundTimeout = 10 * time.Minute

// HandleUpdateApply carries out what HandleUpdateStatus reported.
//
// Returns 202 and does the work in a detached goroutine: the successful path
// ends with this process being SIGTERMed, so a synchronous handler would die
// before its response reached the browser. The client learns the outcome by
// polling GET.
func (h *Handlers) HandleUpdateApply(w http.ResponseWriter, r *http.Request) {
	// CSRF is covered by the auth middleware's Origin check on non-safe methods.
	if !h.installEnabled {
		http.Error(w, "dashboard install disabled (update.dashboard_install=false)", http.StatusForbidden)
		return
	}
	// Throttle before the state checks; a rejected attempt consuming the
	// window is intended.
	if h.applyLimiter != nil && !h.applyLimiter.Allow(updateApplyKey) {
		http.Error(w, "too many update attempts; wait a moment", http.StatusTooManyRequests)
		return
	}
	if h.updateChecker == nil {
		http.Error(w, "auto-update is disabled (update.enabled=false)", http.StatusConflict)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	var req updateApplyRequest
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	snap := h.updateStatus.Snapshot()
	if snap.Current == "" {
		snap.Current = h.buildVersion
	}
	action := snap.Action()
	if action == selfupdate.ActionNone {
		http.Error(w, "nothing to apply", http.StatusConflict)
		return
	}
	if req.ConfirmAction != string(action) {
		http.Error(w, fmt.Sprintf("state changed: confirmed %q but current action is %q",
			req.ConfirmAction, action), http.StatusConflict)
		return
	}
	// Restart only if a managed service runs THIS process (else the UI would
	// strand on "restarting"). One probe, shared with the preflight gate.
	restart := selfupdate.ServiceManagesThisProcess()
	if pre := selfupdate.CheckPreflight(action, snap.Current, restart); !pre.CanApply {
		http.Error(w, pre.Reason, http.StatusConflict)
		return
	}

	apply := h.applyFn
	if apply == nil {
		apply = h.updateChecker.InstallLatest
	}
	slog.Info("dashboard update: apply requested",
		"action", action, "current", snap.Current, "latest", snap.Latest,
		"staged", snap.Staged, "restart", restart,
		"running_sessions", h.runningSessionCount())

	// Write AND FLUSH the 202 before the work starts: net/http buffers the
	// response until the handler returns, and on the restart path the goroutine
	// can SIGTERM this process within milliseconds — the browser would see a
	// dropped connection ("升级请求失败") at the exact moment it is succeeding.
	httputil.WriteJSONStatus(w, http.StatusAccepted, map[string]string{"status": "started"})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	go func() {
		// Detached goroutine: nothing above recovers a panic, so it would take
		// the process down mid-upgrade. MarkFailed so the chip does not stay
		// parked on "installing".
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("dashboard update: apply goroutine panicked", "action", action, "panic", rec)
				h.updateStatus.MarkFailed(fmt.Errorf("apply aborted: %v", rec))
			}
		}()
		// context.Background, NOT r.Context(): the request is already finished
		// and its context cancelled.
		ctx, cancel := context.WithTimeout(context.Background(), applyBackgroundTimeout)
		defer cancel()
		if err := apply(ctx, restart); err != nil {
			// Status already carries phase + last_error for the next poll.
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
// Phase, which the dashboard should see as "idle".
func phaseOrIdle(p selfupdate.Phase) selfupdate.Phase {
	if p == "" {
		return selfupdate.PhaseIdle
	}
	return p
}

// runningSessionCount reports how many sessions are mid-turn so the restart
// confirmation can state the blast radius. A nil router yields 0.
//
// StateSpawning also stringifies to "running"; that is intended — a spawning
// session is in-flight work too.
func (h *Handlers) runningSessionCount() int {
	if h.router == nil {
		return 0
	}
	running := cli.StateRunning.String()
	n := 0
	for _, snap := range h.router.ListSessions() {
		if snap.State == running {
			n++
		}
	}
	return n
}
