// Package session router lifecycle methods.
//
// This file holds the spawn path: GetOrCreate, resolveSpawnParams,
// reserveSpawn, completeSpawn and installFreshSession. The reset family is in
// router_reset.go, RenameSession in router_rename.go, the old session's
// respawn snapshot in respawn_snapshot.go and the spawn helpers in
// spawn_config.go; router_core.go retains the Router struct and NewRouter.
// Functions that need the session table's lock take a sessTx / sessView,
// which only exists inside a table transaction.
package session

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/history"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/shim"
)

// publishSession is the single funnel for installing a freshly-built
// ManagedSession into the router's lookup tables (attachHistorySource →
// session table Put). Every spawn / discovery / rename / takeover path
// must go through it so no site can forget the history source, which would
// leave the dashboard "history" drawer silently blank. Callers that already
// attached the source pass alreadyAttached=true to avoid double-attach.
//
// Post-condition: s.loadHistorySource() is non-nil (Noop at worst).
func (r *Router) publishSession(tx sessTx, key string, s *ManagedSession, alreadyAttached bool) {
	if !alreadyAttached {
		r.hist.attachHistorySource(s, r.backends.sourceWrapperFor(s.Backend()))
	}
	if s.loadHistorySource() == nil {
		// Defence-in-depth against a mistaken alreadyAttached=true: log the
		// diagnostic and install a Noop so downstream callers need not nil-check.
		slog.Error("publishSession: history source missing after attach — falling back to Noop",
			"key", key, "alreadyAttached", alreadyAttached)
		s.SetHistorySource(history.Noop{})
	}
	// A struct replacing key's entry (a respawn) is the same logical session
	// and shares its gap fill cell.
	if prev := tx.Get(key); prev != nil && prev != s && s.gapFill.Load() == nil {
		s.gapFill.Store(prev.gapFillCell())
	}
	// A live session ends key's run of failed spawns, or the run would pause
	// that session's respawn once it dies.
	if s.isAlive() {
		tx.Ext().spawns.ClearStartupFailure(key)
	}
	tx.Put(key, s)
}

// GetOrCreate returns an existing session or creates a new one.
// AgentOpts overrides the router defaults for model and args. A dead session
// whose resume target is gone comes back as SessionResumeLost.
func (r *Router) GetOrCreate(ctx context.Context, key string, opts AgentOpts) (*ManagedSession, SessionStatus, error) {
	// Flag-injection guard: opts.Model originates from dashboard WS, upstream
	// RPC, or planner config and must be validated at the router boundary.
	if err := validateModel(opts.Model); err != nil {
		return nil, 0, err
	}
	// Backend flows into slog attrs and persisted state JSON; this gate only
	// rejects shape-invalid input (wrapperFor tolerates unknown backends).
	if err := validateBackend(opts.Backend); err != nil {
		return nil, 0, err
	}
	// Fast path: a live session needs only a read of the table
	// (touchLastActive is an atomic store), so concurrent hits do not
	// serialise on the write lock.
	if s := r.ss.Load(key); s != nil && s.isAlive() {
		s.touchLastActive()
		return s, SessionExisting, nil
	}
	// N concurrent GetOrCreate on the same fresh key would each spawn, and
	// only one would win the shim-socket dial guard. Each round is one
	// transaction deciding between: the live session, waiting on a spawn
	// already in flight (outside the lock, then another round), or reserving
	// a spawn of our own — so the decision to spawn and the in-flight marker
	// cannot be split by another caller.
	var staleSocketBound bool
	// retryStuck: the shim socket outlived the wait before a rejected
	// resume's fresh retry.
	var retryStuck bool
	for {
		// Only the round right after a stale one inherits its bound socket; a
		// round that waits on another caller's spawn consumes it.
		wrapStale := staleSocketBound
		staleSocketBound = false
		var (
			live      *ManagedSession
			wait      chan struct{}
			res       spawnReservation
			err       error
			status    SessionStatus
			stuck     bool
			resumedID string
		)
		r.ss.Update(func(tx sessTx) {
			if s, ok := tx.Lookup(key); ok {
				if s.isAlive() {
					s.touchLastActive()
					live = s
					return
				}
				// The resume branch honours the SAME coalesce guard as the
				// not-found path: a second concurrent spawn would reuse the
				// in-flight channel and close it twice (#2221). The winner
				// owns the close.
				if ch, inflight := tx.Ext().spawns.SpawnInFlight(key); inflight {
					wait = ch
					return
				}
				resumedID, status = s.getSessionID(), SessionResumed
				if err = startupBreaker(tx, key, time.Now()); err != nil {
					return
				}
				err = r.reserveSpawn(tx, &res, key, resumedID, opts)
				// A resume the guard kept continues this entry's conversation,
				// so it is only valid while this entry is still the key's.
				res.resumesOld = err == nil && res.resumeID != ""
				return
			}
			if ch, inflight := tx.Ext().spawns.SpawnInFlight(key); inflight {
				wait = ch
				return
			}
			if err = startupBreaker(tx, key, time.Now()); err != nil {
				return
			}
			// Consume the per-key shim-stuck flag (set by a Reset whose
			// socket-gone wait timed out, #1324) in the same transaction that
			// reserves the spawn; apply the wrap on the error path.
			stuck, status = tx.Ext().spawns.ConsumeShimStuck(key), SessionNew
			err = r.reserveSpawn(tx, &res, key, "", opts)
		})
		switch {
		case live != nil:
			return live, SessionExisting, nil
		case wait != nil:
			// Someone else is spawning this key: wait, then re-evaluate (pick
			// up their session, or spawn our own on their failure).
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-wait:
			}
			continue
		}
		var s *ManagedSession
		if err == nil {
			if status == SessionResumed {
				slog.Info("session process exited, resuming", "key", key, "session_id", resumedID)
			} else {
				// Debug, not Info: completeSpawn logs "session spawned" at Info
				// moments later.
				slog.Debug("creating new session", "key", key)
			}
			s, err = r.completeSpawn(ctx, &res)
		}
		if errors.Is(err, errSpawnStale) {
			// The resumed entry was reset, removed or replaced meanwhile:
			// decide again against the table as it is now.
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
			staleSocketBound = res.socketBound
			continue
		}
		// Nothing was sent yet, so a resume the backend refused is retried
		// fresh at once; resolveSpawnParams drops it for the flagged session.
		// The failed spawn's shim is still releasing the key's socket, and the
		// retry's StartShim would refuse to clobber it.
		if errors.Is(err, clierr.ErrResumeRejected) && res.old != nil && !res.old.resumeRejected.Swap(true) {
			retryStuck = !waitSocketGoneForKey(key, 2*time.Second)
			continue
		}
		if err != nil {
			if stuck || wrapStale || retryStuck {
				// errors.Is chain lets callers pin on ErrShimStuck.
				return nil, 0, fmt.Errorf("session %s: %w: %w", key, ErrShimStuck, err)
			}
			return nil, 0, fmt.Errorf("session %s: %w", key, err)
		}
		// The resume guard dropped the transcript of a session that had one:
		// the respawn is fresh and the conversation's context is gone.
		if status == SessionResumed && resumedID != "" && res.resumeID == "" && !res.yielded {
			status = SessionResumeLost
		}
		return s, status, nil
	}
}

// spawnParams carries the pure-computation output of resolveSpawnParams:
// the merged backend, model, args, workspace, and (possibly downgraded)
// resumeID that reserveSpawn feeds into cli.SpawnOptions.
type spawnParams struct {
	BackendID string // effective backend ID after override/fallback resolution
	Wrapper   *cli.Wrapper
	Model     string
	Args      []string
	// Effort is the resolved thinking-effort tier ("" = pass no flag).
	Effort string
	// SystemPrompt is AgentOpts.SystemPrompt passed through unchanged; there
	// is no backend- or router-level tier for it.
	SystemPrompt string
	Workspace    string
	// ResumeID after workspace/jsonl guard. Empty means "spawn fresh".
	ResumeID string
	// AccessProfileID is the resolved access-profile name ("" = global
	// default), recorded on the session and persisted so a post-restart
	// resume relocks the same auth chain.
	AccessProfileID string
	// AccessProfileEnv is the RAW profile env map (still holding *_FILE
	// references); nil when AccessProfileID is "". completeSpawn expands it
	// outside the transaction — file reads must not happen under the lock.
	AccessProfileEnv map[string]string
	// Overlay is the per-request layer that went into Model/Effort/Args. The
	// shim persists it so the arg-drift comparison on the next restart can
	// re-merge it against current config (#2494). Always populated: a spawn
	// with no overrides yields the zero value ("known and empty").
	Overlay shim.SpawnOverlay
}

// resolveSpawnParams computes the merged spawn parameters for a new
// session. It is the SINGLE source of truth for workspace + backend + model +
// args + resumeID resolution; any spawn-adjacent path (Takeover, Reattach…)
// MUST route through it rather than re-implement the precedence (#735).
// workspace_resolver_contract_test.go asserts exactly one
// `workspace = opts.Workspace` site survives in the package. No I/O beyond
// bounded stat/ReadDir probes; consumes the one-shot dashboard backend pick.
func (r *Router) resolveSpawnParams(tx sessTx, key, resumeID string, opts AgentOpts) spawnParams {
	// One registry snapshot for the whole resolution: the overlay env and the
	// profile default model must come from the same map.
	profiles := r.backends.profiles()
	// Backend precedence: opts.Backend > one-shot backendOverrides[key]
	// (consumed here) > existing session's Backend (resume continuity) >
	// defaultBackend. Without the existing-session tier a dead kiro session
	// would respawn on the default backend, fail the resume probe, and
	// silently downgrade to a fresh claude session.
	reqBackend := opts.Backend
	if len(tx.Ext().picks.backend) > 0 {
		if reqBackend == "" {
			reqBackend = tx.Ext().picks.backend[key]
		}
		delete(tx.Ext().picks.backend, key)
	}
	if reqBackend == "" {
		if old := tx.Get(key); old != nil {
			if b := old.Backend(); b != "" {
				reqBackend = b
			}
		}
	}
	wrapper, backendID := r.backends.wrapperFor(reqBackend)

	// Access-profile precedence (RFC project-access-profile §2/§7): existing
	// session's recorded profile (RESUME LOCK — a dead session must resume on
	// the SAME auth chain; re-resolving would cross accounts) > one-shot
	// dashboard override (consumed here) > opts.AccessProfile > "". An unknown
	// ID resolves to "" with a warning: the SAFE default, never a wrong account.
	accessProfileID := opts.AccessProfile
	if len(tx.Ext().picks.accessProfile) > 0 {
		if ov, ok := tx.Ext().picks.accessProfile[key]; ok {
			accessProfileID = ov
			delete(tx.Ext().picks.accessProfile, key)
		}
	}
	if old := tx.Get(key); old != nil {
		if ap := old.AccessProfile(); ap != "" {
			accessProfileID = ap
		}
	}
	// defaultAccessProfile is the lowest tier: applies ONLY when every source
	// above left the ID empty, so picks and resume-locked profiles always win.
	if accessProfileID == "" && r.backends.defaultAccessProfile != "" {
		accessProfileID = r.backends.defaultAccessProfile
	}
	var accessProfileEnv map[string]string
	if accessProfileID != "" {
		if ap, ok := profiles[accessProfileID]; ok {
			accessProfileEnv = ap.Env
		} else {
			slog.Warn("access profile not found; falling back to global default",
				"key", key, "access_profile", accessProfileID)
			accessProfileID = ""
		}
	}

	// Per-request overlay the shim persists for the drift re-merge (#2494).
	// AccessProfile is the RESOLVED id so the drift side resolves default_model
	// the same way. ExtraArgs is cloned: the overlay outlives the table lock (JSON-encoded
	// after the unlock), so it must not alias the caller's slice.
	overlay := shim.SpawnOverlay{
		Model:         opts.Model,
		Effort:        opts.Effort,
		ExtraArgs:     slices.Clone(opts.ExtraArgs),
		AccessProfile: accessProfileID,
		// Argv-bearing and per-session, so it must ride in the overlay (#2493).
		AppendSystemPrompt: opts.SystemPrompt,
	}

	// mergeArgvLayers is shared verbatim with driftCompareArgs so the two argv
	// cannot diverge. Chain: backend defaults < access-profile default_model <
	// opts < session tuning (operator's dashboard pick for THIS session,
	// tuningspec-validated at write and at store load). Effort deliberately has
	// NO access-profile tier (docs/rfc/kiro-effort-control.md §4.2).
	// A key with no session yet may carry a pre-spawn pick
	// (picks.tuning); completeSpawn consumes it onto the fresh entry.
	var tuningModel, tuningEffort string
	if old := tx.Get(key); old != nil {
		tuningModel, tuningEffort = old.TuningModel(), old.TuningEffort()
	} else if pt, ok := tx.Ext().picks.tuning[key]; ok {
		tuningModel, tuningEffort = pt.Model, pt.Effort
	}
	merged := mergeArgvLayers(
		r.backends.backendDefaultsFor(backendID),
		profileDefaultModelFor(profiles, accessProfileID),
		overlay, tuningModel, tuningEffort)
	model, effort, args := merged.Model, merged.Effort, merged.Args

	// Workspace: opts override > per-chat override > old session workspace >
	// default. The chat tier goes through resolveWorkspace — the single
	// chat-level resolution point — so it cannot drift from GetWorkspace (#883).
	workspaceOverridden := false
	var workspace string
	if opts.Workspace != "" {
		workspace = opts.Workspace
		workspaceOverridden = true
	} else if chatKey := chatKeyFor(key); chatKey != key {
		workspace = r.resolveWorkspace(tx.View, chatKey)
		// Only an explicit per-chat override pins out the resume tier; a bare
		// default must still let the resume-session workspace win below.
		if _, ok := tx.Ext().workspaces.Lookup(chatKey); ok {
			workspaceOverridden = true
		}
	} else {
		workspace = r.defaultCWD
	}
	if !workspaceOverridden && resumeID != "" {
		if old := tx.Get(key); old != nil {
			if ws := old.Workspace(); ws != "" {
				workspace = ws
			}
		}
	}

	// ResumeID guard: drop when the backend's on-disk resume target is missing
	// so the spawn falls through to a fresh session instead of failing on
	// "No conversation found". The probe is backend-aware (see resolveResumeID).
	resumeID = resolveResumeID(backendID, r.hist.claudeDir, r.hist.backendDirs, workspace, key, resumeID)
	if why, detail := resumeDropReason(tx.Get(key)); resumeID != "" && why != "" {
		slog.Warn("resume failed; starting fresh session", "key", key, "session_id", resumeID,
			"reason", why, "stderr", detail)
		resumeID = ""
	}

	// Canonicalize on-disk case for fresh spawns: on case-insensitive APFS a
	// differently-cased spelling forks two project identities for one tree.
	// Ordering contract: runs AFTER the resume guard, because claude's --resume
	// looks up the jsonl under the slug derived from the exact spelling the
	// previous incarnation stored; re-casing would orphan that transcript.
	if resumeID == "" {
		workspace = osutil.CanonicalCase(workspace)
	}

	return spawnParams{
		BackendID:        backendID,
		Wrapper:          wrapper,
		Model:            model,
		Args:             args,
		Effort:           effort,
		SystemPrompt:     merged.SystemPrompt,
		Workspace:        workspace,
		ResumeID:         resumeID,
		AccessProfileID:  accessProfileID,
		AccessProfileEnv: accessProfileEnv,
		Overlay:          overlay,
	}
}

// sessionOverrides is the operator-owned per-session state that must outlive
// the process it was set on: dashboard tuning override and user label with its
// origin (label+origin travel as one unit). Captured by
// snapshotOldSession and re-applied by installFreshSession.
type sessionOverrides struct {
	tuningModel  string
	tuningEffort string
	userLabel    string
	labelOrigin  string
}

// consumePendingTuning moves a pre-spawn tuning pick
// (picks.tuning, recorded by SetSessionTuning for a key with no
// session) onto the overrides installFreshSession will stamp on the
// new entry, and drops the one-shot record. Returns ov unchanged when there
// is none.
func (r *Router) consumePendingTuning(tx sessTx, key string, ov sessionOverrides) sessionOverrides {
	pt, ok := tx.Ext().picks.tuning[key]
	if !ok {
		return ov
	}
	delete(tx.Ext().picks.tuning, key)
	out := ov
	out.tuningModel = pt.Model
	out.tuningEffort = pt.Effort
	return out
}

// spawnReservation is what reserveSpawn hands to completeSpawn: everything
// decided under the lock before the slow, unlocked part of a spawn.
type spawnReservation struct {
	key      string
	resumeID string
	opts     AgentOpts
	doneCh   chan struct{}
	// guard is the in-flight marker the caller installed before reserving
	// (ResetAndRecreate, across its unlocked close); reserveSpawn takes it
	// over. Nil when the reservation installs its own.
	guard     chan struct{}
	slot      pendingSpawnSlot
	spawnOpts cli.SpawnOptions

	wrapper          *cli.Wrapper
	backendID        string
	workspace        string
	accessProfileID  string
	accessProfileEnv map[string]string

	// old is the session being replaced (nil for a fresh key), snapshotted
	// in the same critical section.
	old  *ManagedSession
	snap respawnSnapshot

	// yielded is set by completeSpawn when it returned a live session another
	// path installed meanwhile instead of its own, so the reservation's resume
	// facts do not describe the returned session.
	yielded bool
	// resumesOld marks a spawn resuming old's own session ID (GetOrCreate's
	// resume branch). Such a spawn is stale once old is no longer the key's
	// entry; a takeover resumes an ID its caller supplied and is never stale.
	resumesOld bool
	// socketBound comes with errSpawnStale when the discarded process's shim
	// socket outlived the wait; the caller's retry wraps its spawn error as
	// ErrShimStuck.
	socketBound bool
	// rejectedResumeID is the transcript a Takeover's refused resume would
	// have continued; the fresh spawn chains it and loads its history.
	rejectedResumeID string
}

// errSpawnStale is completeSpawn's answer for a resumesOld spawn whose entry
// was reset, removed or replaced before the commit: the process, started on
// that entry's conversation, was closed and nothing was installed.
var errSpawnStale = errors.New("the resumed session left the table during the spawn")

// reserveSpawn is the first phase of a spawn, run inside the caller's
// transaction so the decision to spawn and the in-flight marker are one
// critical section: the shutdown gate, the in-flight marker, capacity
// (evicting at capacity, which closes the victim through tx.Unlocked), spawn
// params, the pending slot and the snapshot of the session being replaced.
// It fills res, which the caller owns so the reservation is never copied.
// On error nothing is left reserved. completeSpawn runs the rest outside it.
func (r *Router) reserveSpawn(tx sessTx, res *spawnReservation, key, resumeID string, opts AgentOpts) error {
	// Shutdown gate (#1822): r.stopped is set in the same transaction as
	// Shutdown's snapshot, so gate and snapshot are mutually exclusive and a
	// late spawn cannot install a shim+CLI the snapshot missed.
	if r.stopped.Load() {
		return ErrRouterStopped
	}

	// Mark this key as spawning so ReconnectShims does not treat the fresh
	// shim's state file as an orphan, and concurrent GetOrCreates park on the
	// done-channel instead of spawning too. A guard the caller pre-installed
	// (res.guard, ResetAndRecreate) is reused so the marker stays continuous
	// (#775); anyone else's in-flight spawn is refused, not joined — joining
	// runs two spawns for one key and ends the guard twice. From here on any
	// failure, error or panic, ends the marker so no waiter is left parked.
	doneCh, owned := tx.Ext().spawns.BeginSpawn(key)
	if !owned && doneCh != res.guard {
		return ErrSpawnInFlight
	}
	reserved := false
	defer func() {
		if !reserved {
			tx.Ext().spawns.EndSpawn(key, doneCh)
		}
	}()

	// Exempt sessions (planners) bypass maxProcs but have their own limit.
	if !opts.Exempt {
		// Only recount (O(n)) when we appear to be at capacity, to detect
		// drift from undetected process exits before refusing. int64 locals
		// avoid 32-bit wrap.
		maxProcs64 := int64(r.maxProcs)
		pending64 := int64(tx.Ext().spawns.PendingSpawns())
		if tx.Active()+pending64 >= maxProcs64 {
			r.countActive(tx)
		}
		if tx.Active()+pending64 >= maxProcs64 {
			if !r.evictOldest(tx) {
				return fmt.Errorf("%w (%d), all busy", ErrMaxProcs, r.maxProcs)
			}
			// evictOldest releases the lock around the victim's Close(), so
			// pendingSpawns may have changed; re-read it or a stale value
			// over-spawns past maxProcs / falsely refuses (#2082).
			pending64 = int64(tx.Ext().spawns.PendingSpawns())
			if tx.Active()+pending64 >= maxProcs64 {
				return fmt.Errorf("%w (%d), all busy", ErrMaxProcs, r.maxProcs)
			}
		}
	} else {
		// Per-namespace sub-quota runs FIRST so a noisy cron chat cannot push
		// planner / sys stubs out of the shared pool; the global
		// maxExemptSessions ceiling is a relief valve for namespaces without
		// sub-quota wiring. One combined walk yields both counts.
		kind := exemptKind(key)
		perKind, totalExempt := countExemptCombined(tx.View, kind)
		if kind != "" && perKind >= exemptCapFor(kind) {
			return fmt.Errorf("%w: %s namespace (%d)", ErrMaxExemptSessions, kind, exemptCapFor(kind))
		}
		if totalExempt >= maxExemptSessions {
			return fmt.Errorf("%w (%d)", ErrMaxExemptSessions, maxExemptSessions)
		}
	}

	// Consumes the one-shot backend pick for `key`.
	sp := r.resolveSpawnParams(tx, key, resumeID, opts)
	res.key, res.resumeID, res.opts, res.doneCh = key, sp.ResumeID, opts, doneCh
	res.wrapper, res.backendID, res.workspace = sp.Wrapper, sp.BackendID, sp.Workspace
	res.accessProfileID, res.accessProfileEnv = sp.AccessProfileID, sp.AccessProfileEnv
	// argv-bearing fields come from the shared constructor so this path and
	// the arg-drift comparison cannot diverge. DebugFile uses the
	// side-effecting cliDebugFileFor (log pre-created 0600) where drift uses
	// the read-only cliDebugPathFor.
	res.spawnOpts = r.spawn.argvSpawnOptions(sp.Model, sp.Effort, r.spawn.cliDebugFileFor(key), sp.SystemPrompt, sp.Args)
	// ResumeID is session state, not config: the drift side strips it
	// (stripResumeArgs) so a resumed session is not read as drift.
	res.spawnOpts.ResumeID = res.resumeID
	// Always non-nil: an empty overlay must be recorded as "known and empty"
	// or the drift check treats this shim as legacy (#2494).
	spawnOverlay := sp.Overlay
	res.spawnOpts.SpawnOverlay = &spawnOverlay
	// Process wiring BuildArgs never reads.
	res.spawnOpts.Key = key
	res.spawnOpts.WorkingDir = res.workspace
	res.spawnOpts.NoOutputTimeout = r.spawn.noOutputTimeout
	res.spawnOpts.TotalTimeout = r.spawn.totalTimeout

	// The snapshot is taken in this same critical section. The pending slot,
	// taken last, keeps a concurrent Cleanup from pruning the slot we are about
	// to fill; from here on completeSpawn owns the marker and the slot.
	res.old = tx.Get(key)
	res.snap = snapshotRespawn(tx.View, res.old)
	res.slot = r.acquirePendingSpawnSlot(tx)
	reserved = true
	return nil
}

// completeSpawn runs a reserved spawn to the end, outside any transaction:
// the env overlay, Spawn — which may block on an ACP handshake — and the
// previous-history copy, then a commit transaction that re-checks the key and
// installs. The in-flight marker and the pending slot are released however it
// returns.
func (r *Router) completeSpawn(ctx context.Context, res *spawnReservation) (_ *ManagedSession, err error) {
	key := res.key
	// One transaction on the way out ends the spawn marker and, when the
	// commit did not already, releases the slot. Panic-safe: a panicking
	// Spawn must still decrement pendingSpawns or the router permanently
	// refuses new sessions with ErrMaxProcs. A failed Init handshake is
	// recorded before the marker ends, so the waiters it wakes are paused.
	defer r.ss.Update(func(tx sessTx) {
		res.slot.releaseIn(tx)
		if countsAsStartupFailure(ctx, err) {
			noteSpawnFailure(tx, key, err, time.Now())
		}
		tx.Ext().spawns.EndSpawn(key, res.doneCh)
	})

	if res.wrapper == nil {
		return nil, fmt.Errorf("spawn process (backend %q): %w", res.backendID, ErrNoCLIWrapper)
	}
	// Expand the access-profile env overlay outside the lock (reads *_FILE
	// secrets from disk). FAIL-LOUD on a missing secret — silently spawning on
	// the global default would run this session on the wrong account.
	if len(res.accessProfileEnv) > 0 {
		overlay, err := resolveEnvOverlay(res.accessProfileEnv)
		if err != nil {
			return nil, fmt.Errorf("access profile %q: %w", res.accessProfileID, err)
		}
		res.spawnOpts.EnvOverlay = overlay
	}
	proc, err := r.spawn.spawnProcess(ctx, res.wrapper, res.spawnOpts, key, res.backendID)
	if err != nil {
		return nil, fmt.Errorf("spawn process: %w", err)
	}
	// historyMu must not nest inside the table lock (event injection takes it
	// on its own), so the old session's history is copied here. The old
	// reference is safe to read: sessions are never mutated after creation,
	// only replaced.
	old, snap := res.old, res.snap
	hist := collectRespawnHistory(old, snap, res.resumeID)
	costBase := resumedCostBaseline(r.hist.claudeDir, r.hist.backendDirs, res)

	var s, winner *ManagedSession
	var prevIDs []string
	var oldHistory []clievent.EventEntry
	var stale bool
	r.ss.Update(func(tx sessTx) {
		res.slot.releaseIn(tx)
		// The CLI got past Init: the key's run of failed spawns ends here and
		// carries on in the new session's startupFails.
		failedSpawns, _ := tx.Ext().spawns.StartupFailure(key)
		tx.Ext().spawns.ClearStartupFailure(key)
		for {
			// A concurrent spawn may have installed a live session for this
			// key while we were unlocked; if so it wins and ours is closed.
			cur := tx.Get(key)
			if cur != nil && cur.isAlive() {
				winner = cur
				return
			}
			if cur == old {
				break
			}
			// The key's entry was removed or replaced by a dead one meanwhile.
			// A process resuming the old entry's conversation would bring a
			// reset session back, so it is discarded. Otherwise the process is
			// not tied to the old entry (fresh, or a caller-supplied resume):
			// it continues the entry there now, so a removed session is not
			// resurrected into this one.
			if res.resumesOld {
				stale = true
				return
			}
			old = cur
			snap = snapshotRespawn(tx.View, old)
			tx.Unlocked(func() { hist = collectRespawnHistory(old, snap, res.resumeID) })
		}
		rereadSameEntry(old, &snap, &hist, res.resumeID)
		// A key with no session yet takes its pre-spawn tuning pick now, once
		// the spawn has succeeded, so a failed spawn leaves the pick for the
		// retry.
		overrides := snap.overrides
		if old == nil {
			overrides = r.consumePendingTuning(tx, key, overrides)
		}
		oldHistory, prevIDs = hist.entries, hist.prevIDs
		s = r.installFreshSession(tx,
			key, proc, res.workspace, res.backendID, res.accessProfileID, res.wrapper, res.resumeID,
			oldHistory, respawnChain(prevIDs, res.rejectedResumeID, ""), snap.cost, snap.costSpent, snap.createdAt, res.opts.Exempt, snap.sid,
			hist.userTurns, overrides,
		)
		s.startupFails.Store(max(snap.startupFails, failedSpawns.Streak))
		s.costMu.Lock()
		s.spent = snap.spent
		costBase.applyLocked(s)
		s.costMu.Unlock()
	})
	if winner != nil {
		proc.Close()
		res.yielded = true
		return winner, nil
	}
	if stale {
		// The in-flight marker is still held here, so other callers for key
		// stay parked until the socket is gone.
		res.socketBound = !discardStaleSpawn(key, res.resumeID, proc)
		return nil, errSpawnStale
	}
	// The argv was built from the reserve-time tuning; SetSessionTuning
	// already told the caller a pick made meanwhile is deferred.
	if old != nil && (snap.overrides.tuningModel != res.snap.overrides.tuningModel ||
		snap.overrides.tuningEffort != res.snap.overrides.tuningEffort) {
		slog.Info("session tuning picked during the spawn applies on the next spawn",
			"key", osutil.SanitizeForLog(key, 64))
	}

	// The loader appends its resume ID to prevIDs itself.
	r.hist.bindNewSessionHistory(ctx, s, proc, key, cmp.Or(res.resumeID, res.rejectedResumeID), res.workspace, prevIDs, oldHistory)
	r.notifyChange()
	return s, nil
}

// discardStaleSpawn closes a stale spawn's process and waits for its shim
// socket to go, so the next spawn for key does not hit the "refusing to
// clobber" guard. False means the socket is still bound.
func discardStaleSpawn(key, resumeID string, proc processIface) bool {
	slog.Info("resumed session left the table during the spawn; spawning again",
		"key", osutil.SanitizeForLog(key, 64), "resume_id", resumeID)
	proc.Close()
	if waitSocketGoneForKey(key, 2*time.Second) {
		return true
	}
	slog.Warn("shim socket still bound after discarding a stale spawn — the retry's spawn error will be wrapped as ErrShimStuck",
		"key", osutil.SanitizeForLog(key, 64))
	return false
}

// installFreshSession attaches a freshly-spawned process to the router
// indices + event log. Pure state mutation, no I/O. Callers must invoke
// installPersistSink AFTER this returns (RFC §3.2.2).
func (r *Router) installFreshSession(tx sessTx,
	key string,
	proc processIface,
	workspace string,
	backendID string,
	accessProfileID string,
	wrapper *cli.Wrapper,
	resumeID string,
	oldHistory []clievent.EventEntry,
	prevIDs []string,
	oldTotalCost float64,
	oldCostSpent float64,
	oldCreatedAt int64,
	exempt bool,
	oldSID string,
	oldUserTurns int64,
	overrides sessionOverrides,
) *ManagedSession {
	s := &ManagedSession{
		key:              key,
		persistedHistory: oldHistory,
		prevSessionIDs:   prevIDs,
		exempt:           exempt,
		runStore:         r.runs.runs,
		costAcct:         r.runs.cost,
		// Runs later, when the CLI reports its session ID: its own transaction.
		onSessionID: func(id string) {
			r.ss.Update(func(tx sessTx) {
				r.kid.Track(id)
				tx.SetID(id, key)
			})
		},
	}
	// Seed persistedUserTurns so the proc==nil snapshot branch and AutoTitler
	// min-turn gate see the correct count immediately; s is unpublished so
	// there are no concurrent readers. oldUserTurns was computed by
	// collectPreviousHistory (#2089).
	if len(oldHistory) > 0 {
		s.persistedUserTurns.Store(oldUserTurns)
	}
	storeTotalCost(&s.totalCost, oldTotalCost)
	// lastCumulativeCost starts at 0 here; a resumed CLI does not count from
	// 0, so completeSpawn then installs the cost it restores (resumed_cost.go).
	storeTotalCost(&s.costSpent, oldCostSpent)
	// Sidebar order anchor: inherit oldCreatedAt when replacing a prior incarnation.
	if oldCreatedAt != 0 {
		s.createdAt.Store(oldCreatedAt)
	} else {
		s.initCreatedAtIfUnset()
	}
	s.setWorkspace(workspace)
	s.SetBackend(backendID)
	// Recorded so a later resume relocks the same auth chain (§7).
	s.SetAccessProfile(accessProfileID)
	s.SetCLIName(wrapper.CLIName)
	s.SetCLIVersion(wrapper.CLIVersion)
	// Operator-owned state must outlive the process: this spawn's argv was
	// built from the OLD entry's tuning, and without carrying it the next TTL
	// recycle drops back to config default and a restart reads the shim as
	// arg-drift. Values come from the commit-time read of the entry this
	// session replaces, never from a re-read of tx.Get(key).
	s.SetTuningModel(overrides.tuningModel)
	s.SetTuningEffort(overrides.tuningEffort)
	s.SetUserLabel(overrides.userLabel)
	s.setLabelOrigin(overrides.labelOrigin) // label+origin travel as one unit
	// Serialises storeProcess + seededLen reset under historyMu so a concurrent
	// InjectHistory observes the (process, seededLen) pair and forwards only
	// genuinely-new tail; same lock-protected path as the reconnect branch.
	snapshot := s.attachProcessAndSnapshotPersisted(proc)
	// Notify the dashboard on out-of-band turn completion (as ReconnectShims
	// does). SetOnTurnDone is mu-guarded inside Process, so post-storeProcess is safe.
	if n, ok := proc.(turnDoneNotifier); ok {
		n.SetOnTurnDone(func() { r.notifyChange() })
	}
	bookUnownedResults(s, proc)
	if len(snapshot) > 0 {
		proc.InjectHistory(snapshot)
	}
	// Prefer resumeID, else whatever the protocol captured during Init (ACP
	// returns a UUID synchronously; claude stays empty until the first turn).
	// Without the fallback a fresh kiro session has an empty sessionID and
	// saveStore drops it, losing the session across restarts.
	effectiveSID := resumeID
	if effectiveSID == "" {
		effectiveSID = proc.SessionID()
	}
	s.setSessionID(effectiveSID)
	// When a respawn rotates the SID, drop the old idToKey entry iff it still
	// points at this key, so a resume of the retired SID cannot be mis-routed;
	// the "still maps to key" guard avoids clobbering another live session's
	// entry (#2093).
	if oldSID != "" && oldSID != effectiveSID {
		tx.ClearIDIfOwnedBy(oldSID, key)
	}
	if effectiveSID != "" {
		r.kid.Track(effectiveSID)
		tx.SetID(effectiveSID, key)
	}
	s.touchLastActive()
	r.publishSession(tx, key, s, false)
	if !exempt {
		tx.AddActive(1)
	}

	tx.MarkChanged()
	logSessionLifecycle("spawned", key, "active", tx.Active(), "exempt", exempt)
	// Counters bumped inside the write-lock at the authoritative "spawn
	// succeeded" point. Exempt sessions are excluded: they don't consume a
	// slot and planner/scratch churn would muddy the signal.
	if !exempt {
		metrics.SessionCreateTotal.Add(1)
		// Per-backend mirror of activeCount; decremented at every site that
		// decrements activeCount.
		metrics.RecordSessionActive(s.Backend(), 1)
	}
	return s
}

// unregisterSession removes a session from all routing indexes.
// If keepBackendOverride is true, backendOverrides[key] is preserved so a
// following spawn can consume it in the same transaction (used by
// ResetAndRecreate / Takeover which reuse the same key). On terminal removal
// paths (Reset / Remove / Cleanup prune) pass false to prevent override leaks.
func (r *Router) unregisterSession(tx sessTx, key string, s *ManagedSession, keepBackendOverride bool) {
	if s == nil {
		return
	}
	if id := s.getSessionID(); id != "" {
		tx.ClearID(id)
	}
	tx.Delete(key)
	if !keepBackendOverride {
		// Every pick, so an abandoned choice cannot be consumed by a future
		// session that reuses this key. See pendingPicks for the per-map
		// lifecycles — the comment here used to claim accessProfile shared
		// backendOverrides' lifecycle, which was backwards.
		tx.Ext().picks.dropAll(key)
		// The shim-stuck flag is only consumed by GetOrCreate, so terminal
		// removals must clear it or the entry lives for the process lifetime.
		tx.Ext().spawns.ClearShimStuck(key)
		tx.Ext().spawns.ClearStartupFailure(key)
	}
}
