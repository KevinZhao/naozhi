package session

// router_tuning.go — per-session model/effort override entry point for the
// dashboard override API. docs/rfc/dashboard-model-effort-control.md §4.3/§4.4.
//
// Apply matrix (§4.4), by field × process state:
//
//	model, no effective effort, live proc → RPC (session/set_model /
//	    set_model control_request), no restart. F1/F6/F13/F14.
//	model, effective effort non-empty, EffortTier backend, live proc →
//	    respawn: kiro's set_model resets the tier to the new model's
//	    default and v2 has no runtime restore channel (F9/F4), so the RPC
//	    fast path would silently drop the tier.
//	effort (any change), live proc → respawn.
//	suspended (no live proc) → record only; next spawn injects via flags.
//
// "Respawn" here is the LAZY variant: gracefully Close the CLI process and
// leave the ManagedSession in place, so the next message (or dashboard
// send) resumes through the normal suspended→GetOrCreate path with the new
// argv. This deliberately reuses the TTL-recycle machinery instead of
// spawning eagerly — the queue/drain semantics (RFC §6 R6) come for free,
// and "生效时机 = 下一轮" matches what the popover promises anyway.

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
// dashboard as `applied_via` — the popover renders its ⓘ hint from this
// instead of re-deriving the path client-side (§4.1).
const (
	// TuningAppliedRPC: the live process acknowledged the switch; effective
	// for the next turn (kiro, F13) or possibly the current one (claude, F14).
	TuningAppliedRPC = "rpc"
	// TuningAppliedRespawn: the CLI process was closed; the next message
	// respawns with the new flags, context restored via resume.
	TuningAppliedRespawn = "respawn"
	// TuningAppliedDeferred: recorded only (no live process, or the protocol
	// has no runtime channel); the next spawn applies it.
	TuningAppliedDeferred = "deferred"
)

// ErrTuningUnknownSession is returned when key has no ManagedSession —
// tuning targets an existing conversation, never creates one.
var ErrTuningUnknownSession = errors.New("no session for key")

// ErrTuningEffortUnsupported is returned when an effort tier is set for a
// session whose backend protocol does not honour SpawnOptions.Effort
// (claude/codex). Mapped to 400 by the API handler — silently recording it
// would let the operator believe a tier is in force when it is not (same
// rationale as kiro-effort-control §4.3's warn-and-drop, but this is an
// interactive path where an error is actionable).
var ErrTuningEffortUnsupported = errors.New("backend does not support effort tiers")

// SetSessionTuning validates, records, persists and applies a per-session
// model/effort override. nil = leave the field unchanged; pointer to "" =
// clear the override (config chain reapplies on next spawn); pointer to a
// value = set it.
//
// Returns the apply mode actually taken (TuningApplied*) so the API ack can
// tell the operator what to expect, or an error when nothing was recorded:
//   - validation failure (tuningspec / ErrTuningEffortUnsupported /
//     ErrTuningUnknownSession) → nothing happened;
//   - cli.ErrSetModelRejected (claude org-policy / unknown model, F7/F15) →
//     the override is NOT recorded (§6 R8 ack-before-persist) and the CLI's
//     rejection text is in the error for the dashboard toast.
//
// Concurrency: r.mu is held for the decide+record phase only. The RPC wait
// (up to setModelAckTimeout) and proc.Close run unlocked — a concurrent
// mutation between phases resolves as last-write-wins (RFC §6 R5), with the
// confirmation loop making the final state visible.
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
		r.mu.Unlock()
		return "", ErrTuningUnknownSession
	}
	wrapper, backendID := r.wrapperFor(sess.Backend())
	// A missing wrapper (misconfigured backend id, or a test router without
	// one) degrades to zero caps: recording a model override is still safe
	// (it only feeds the next spawn's flags), while an effort tier — which
	// requires a capability we cannot confirm — is rejected below.
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

	// Effective tier the session will run under AFTER this call — decides
	// the F9 path split. Visible layers: backend default (cli.effort /
	// cli.backends[].effort via backendDefaultsFor) and the tuning override.
	// The agents[].effort layer arrives via AgentOpts at send time and is
	// NOT visible here — an agent-tiered session on an otherwise tier-less
	// kiro backend could take the RPC path and have its agent tier reset by
	// F9 until the next respawn. Same family as the drift check's
	// agent-layer blind spot (kiro-effort-control §4.5.1); accepted and
	// documented rather than plumbed, because AgentOpts construction lives
	// in the dispatch layer.
	effectiveEffort := r.backendDefaultsFor(backendID).Effort
	if effort != nil {
		if *effort != "" {
			effectiveEffort = *effort
		}
	} else if te := sess.TuningEffort(); te != "" {
		effectiveEffort = te
	}

	respawnNeeded := effortChanged || (modelChanged && caps.EffortTier && effectiveEffort != "")

	proc := sess.loadProcess()
	alive := proc != nil && proc.Alive()

	// RPC fast path defers recording until the CLI acks (§6 R8). Every
	// other path records now, under the same r.mu hold as the decision.
	rpcPath := modelChanged && !respawnNeeded && alive
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
			// F7/F15: CLI refused. Nothing was recorded; surface verbatim
			// (text already sanitized at the protocol layer).
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
		// Lazy respawn: stamp a recognisable deathReason and close. The
		// session entry survives, so the next message resumes through
		// GetOrCreate with the new argv (tuning tier sits atop
		// resolveSpawnParamsLocked's chains). Mirrors the TTL-recycle
		// teardown; queued messages drain through the existing
		// suspended→spawn path (RFC §6 R6).
		storeAtomicString(&sess.deathReason, "tuning_respawn")
		proc.Close()
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
