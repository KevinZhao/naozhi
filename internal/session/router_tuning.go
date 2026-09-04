package session

// router_tuning.go — per-session model/effort override entry point for the
// dashboard override API. docs/rfc/dashboard-model-effort-control.md §4.3/§4.4.
//
// Apply matrix (§4.4), by field × process state:
//
//	model, no effective effort, live proc → RPC (set_model), no restart.
//	model, effective effort non-empty, EffortTier backend, live proc →
//	    respawn: kiro's set_model resets the tier to the new model's
//	    default and has no runtime restore channel, so the RPC fast path
//	    would silently drop the tier.
//	effort (any change), live proc → respawn.
//	suspended (no live proc) → record only; next spawn injects via flags.
//
// "Respawn" is LAZY: Close the CLI process and keep the ManagedSession, so
// the next message resumes through the normal suspended→GetOrCreate path
// with the new argv. Reusing the TTL-recycle machinery gives the queue/drain
// semantics (RFC §6 R6) for free and matches the popover's "生效时机 = 下一轮".

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/tuningspec"
)

// Tuning apply modes returned to the override API and surfaced to the
// dashboard as `applied_via` (§4.1).
const (
	// TuningAppliedRPC: the live process acknowledged the switch; effective
	// for the next turn (kiro) or possibly the current one (claude).
	TuningAppliedRPC = "rpc"
	// TuningAppliedRespawn: the CLI process was closed; the next message
	// respawns with the new flags, context restored via resume.
	TuningAppliedRespawn = "respawn"
	// TuningAppliedDeferred: recorded only (no live process, or the protocol
	// has no runtime channel); the next spawn applies it.
	TuningAppliedDeferred = "deferred"
)

// pendingTuning is a model/effort pick recorded for a key that has no
// ManagedSession yet (bkStore.tuningOverrides). A freshly created dashboard
// session exists only client-side until its first message spawns the CLI,
// yet its header chips are already clickable — so the pick is parked here
// and spawnSession moves it onto the new entry. Empty field = no override.
type pendingTuning struct {
	Model  string
	Effort string
}

// maxTuningOverrides caps bkStore.tuningOverrides for the same reason as
// maxBackendOverrides: an authenticated caller can POST unique keys and an
// abandoned pick is only cleared on spawn / Reset / Remove.
const maxTuningOverrides = 1024

// ErrTuningCapacity is returned when a pre-spawn pick cannot be recorded
// because bkStore.tuningOverrides is at maxTuningOverrides.
var ErrTuningCapacity = errors.New("too many pending tuning overrides")

// ErrTuningEffortUnsupported is returned when an effort tier is set for a
// session whose backend protocol does not honour SpawnOptions.Effort (see
// ClaudeProtocol.Capabilities). Mapped to 400 by the API handler — silently
// recording it would let the operator believe a tier is in force when it is not.
var ErrTuningEffortUnsupported = errors.New("backend does not support effort tiers")

// SetSessionTuning validates, records, persists and applies a per-session
// model/effort override. nil = leave unchanged; pointer to "" = clear (config
// chain reapplies on next spawn); pointer to a value = set. Returns the apply
// mode taken (TuningApplied*), or an error when nothing was recorded —
// validation failures, or cli.ErrSetModelRejected, where the override is NOT
// recorded (§6 R8 ack-before-persist) and the CLI's rejection text is in the error.
//
// Concurrency: r.mu is held for the decide+record phase only. The RPC wait
// and proc.Close run unlocked — a concurrent mutation between phases resolves
// as last-write-wins (RFC §6 R5).
func (r *Router) SetSessionTuning(ctx context.Context, key string, model, effort *string) (string, error) {
	if model == nil && effort == nil {
		return "", fmt.Errorf("no tuning field provided")
	}
	if model != nil {
		if err := tuningspec.ValidateModel("tuning model", *model); err != nil {
			return "", err
		}
	}
	if effort != nil {
		if err := tuningspec.ValidateEffort("tuning effort", *effort); err != nil {
			return "", err
		}
	}

	// ---- decide + (conditionally) record, under r.mu ----
	r.mu.Lock()
	sess := r.ss.sessions[key]
	if sess == nil {
		err := r.setPendingTuningLocked(key, model, effort)
		r.mu.Unlock()
		if err != nil {
			return "", err
		}
		slog.Info("session tuning recorded for a session not yet spawned",
			"key", osutil.SanitizeForLog(key, 64), "model_set", model != nil, "effort_set", effort != nil)
		return TuningAppliedDeferred, nil
	}
	wrapper, backendID := r.wrapperFor(sess.Backend())
	// A missing wrapper degrades to zero caps: a model override is still safe
	// to record (only feeds the next spawn's flags); an effort tier needs a
	// capability we cannot confirm, so it is rejected below.
	var caps cli.Caps
	if wrapper != nil {
		caps = cli.ProtocolCaps(wrapper.Protocol)
	}
	if effort != nil && *effort != "" && !caps.EffortTier {
		r.mu.Unlock()
		return "", fmt.Errorf("%w: %s", ErrTuningEffortUnsupported, backendID)
	}

	effortChanged := effort != nil && *effort != sess.TuningEffort()
	modelChanged := model != nil && *model != sess.TuningModel()
	if !effortChanged && !modelChanged {
		r.mu.Unlock()
		return TuningAppliedDeferred, nil // no-op: already at the requested values
	}

	// Effective tier AFTER this call decides the RPC-vs-respawn split. Only
	// the backend default and the tuning override are visible here; the
	// agents[].effort layer arrives via AgentOpts at send time, so an
	// agent-tiered session on a tier-less kiro backend may take the RPC path
	// and lose its agent tier until the next respawn (kiro-effort-control §4.5.1).
	effectiveEffort := r.backendDefaultsFor(backendID).Effort
	if effort != nil {
		if *effort != "" {
			effectiveEffort = *effort
		}
	} else if te := sess.TuningEffort(); te != "" {
		effectiveEffort = te
	}

	// Clearing the model override ("恢复默认") has no model id to hand the CLI
	// (set_model("") blanks kiro's header / is rejected by claude), so it
	// always takes the respawn/deferred path and the config chain reapplies
	// on the next spawn (RFC §4.3 清除语义).
	modelCleared := modelChanged && *model == ""
	respawnNeeded := effortChanged || modelCleared ||
		(modelChanged && caps.EffortTier && effectiveEffort != "")

	proc := sess.loadProcess()
	alive := proc != nil && proc.Alive()

	// RPC fast path defers recording until the CLI acks (§6 R8); every other
	// path records now, under the same r.mu hold as the decision. `*model != ""`
	// is implied by !respawnNeeded but spelled out so "never RPC an empty
	// model" survives refactors.
	rpcPath := modelChanged && *model != "" && !respawnNeeded && alive
	if !rpcPath {
		if modelChanged {
			sess.SetTuningModel(*model)
		}
		if effortChanged {
			sess.SetTuningEffort(*effort)
		}
		r.ss.dirty = true
		r.ss.gen.Add(1)
	}
	r.mu.Unlock()

	logKey := osutil.SanitizeForLog(key, 64)

	// ---- apply, unlocked ----
	if rpcPath {
		err := procSetModel(ctx, proc, *model)
		switch {
		case err == nil:
			// Ack success → record + persist (the only deferred-record path).
			r.mu.Lock()
			if cur := r.ss.sessions[key]; cur != nil {
				cur.SetTuningModel(*model)
				cur.SetModel(*model) // persisted display mirror (F11)
				r.ss.dirty = true
				r.ss.gen.Add(1)
			}
			r.mu.Unlock()
			r.notifyChange()
			slog.Info("session tuning applied via rpc", "key", logKey, "model", *model)
			return TuningAppliedRPC, nil
		case errors.Is(err, cli.ErrSetModelRejected):
			// CLI refused. Nothing was recorded; surface verbatim (text
			// already sanitized at the protocol layer).
			slog.Warn("session tuning rejected by CLI", "key", logKey, "err", err)
			return "", err
		default:
			// Transport failure / no runtime channel / pre-handshake / ack
			// timeout: degrade to record-only, applies on next spawn
			// (§4.4 "失败则降级为记 override + 提示下次生效").
			r.mu.Lock()
			if cur := r.ss.sessions[key]; cur != nil {
				cur.SetTuningModel(*model)
				r.ss.dirty = true
				r.ss.gen.Add(1)
			}
			r.mu.Unlock()
			r.notifyChange()
			slog.Warn("session tuning rpc failed; recorded for next spawn",
				"key", logKey, "err", err)
			return TuningAppliedDeferred, nil
		}
	}

	if respawnNeeded && alive {
		// Lazy respawn: stamp a recognisable deathReason and close; the
		// session entry survives so the next message resumes through
		// GetOrCreate with the new argv. Queued messages drain through the
		// existing suspended→spawn path (RFC §6 R6).
		storeAtomicString(&sess.deathReason, "tuning_respawn")
		proc.Close()
		// Recount (not activeCount.Add(-1)) so the per-backend gauge and total
		// stay in lockstep with the map; otherwise every tuning respawn leaks
		// one slot. Broadcast under r.mu so Shutdown's cond.Wait predicate
		// cannot re-evaluate between Close and the wakeup.
		r.mu.Lock()
		if r.shutdownCond != nil {
			r.shutdownCond.Broadcast()
		}
		r.countActive()
		r.mu.Unlock()
		r.notifyChange()
		slog.Info("session tuning applied via respawn", "key", logKey,
			"model_changed", modelChanged, "effort_changed", effortChanged)
		return TuningAppliedRespawn, nil
	}

	r.notifyChange()
	slog.Info("session tuning recorded (deferred to next spawn)", "key", logKey,
		"model_changed", modelChanged, "effort_changed", effortChanged)
	return TuningAppliedDeferred, nil
}

// procSetModel adapts the processIface (which test fakes implement without a
// SetModel method) to the live *cli.Process SetModel. A process that cannot
// switch models at runtime — codex protocol, or a fake — reports
// ErrSetModelUnsupported and the caller degrades to the deferred path.
func procSetModel(ctx context.Context, proc processIface, model string) error {
	ms, ok := proc.(interface {
		SetModel(ctx context.Context, model string) error
	})
	if !ok {
		return cli.ErrSetModelUnsupported
	}
	return ms.SetModel(ctx, model)
}

// setPendingTuningLocked records (or clears) the pre-spawn pick for key.
// nil leaves a field as is; "" clears it; a record with both fields empty is
// deleted so a "恢复默认" on a never-spawned session leaves no residue.
// Caller holds r.mu.
func (r *Router) setPendingTuningLocked(key string, model, effort *string) error {
	cur, exists := r.bkStore.tuningOverrides[key]
	if model != nil {
		cur.Model = *model
	}
	if effort != nil {
		cur.Effort = *effort
	}
	if cur.Model == "" && cur.Effort == "" {
		delete(r.bkStore.tuningOverrides, key)
		return nil
	}
	if !exists && len(r.bkStore.tuningOverrides) >= maxTuningOverrides {
		return ErrTuningCapacity
	}
	if r.bkStore.tuningOverrides == nil {
		r.bkStore.tuningOverrides = make(map[string]pendingTuning)
	}
	r.bkStore.tuningOverrides[key] = cur
	return nil
}
