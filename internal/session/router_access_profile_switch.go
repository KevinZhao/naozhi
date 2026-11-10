package session

// router_access_profile_switch.go — moving a session that has already talked
// onto another access profile (RFC project-access-profile §7.1): the account
// it bills and, optionally, the model it runs, keeping its key, history and
// Claude session ID.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/tuningspec"
)

// ErrAccessProfileSwitchBusy refuses a switch while the session's turn runs:
// the switch closes the CLI, which would cut the turn off.
var ErrAccessProfileSwitchBusy = errors.New("session is running a turn; switch the access profile once it ends")

// ErrAccessProfileUnknown refuses a switch to a profile that is not configured.
var ErrAccessProfileUnknown = errors.New("access profile is not configured")

// SwitchAccessProfile moves key onto access profile profile ("" = the global
// credentials) and, when model is non-nil, onto that model too (tuning
// semantics: "" clears the session's model override). It is the one
// sanctioned way to change a session's recorded profile, which a spawn
// otherwise keeps across resumes (the resume lock in resolveSpawnParams).
//
// The profile's env and credential source are fixed for the life of a CLI
// process, so a live process is closed and the next message resumes the same
// Claude session ID under the new profile, writing a fresh session settings
// file (TuningAppliedRespawn); a session with no live process just records it
// (TuningAppliedDeferred). A key with no session parks both as the pre-spawn
// picks. A running turn is refused, never cut off.
//
// Choosing "" lets the key's own profile (project binding, agent, default)
// apply again on the next spawn, as for a session that never picked one.
func (r *Router) SwitchAccessProfile(ctx context.Context, key, profile string, model *string) (string, error) {
	if profile != "" && !r.backends.HasAccessProfile(profile) {
		return "", fmt.Errorf("%w: %q", ErrAccessProfileUnknown, profile)
	}
	if model != nil {
		if err := tuningspec.ValidateModel("model", *model); err != nil {
			return "", err
		}
	}
	aws := r.backends.accessProfileAWS(profile)

	var (
		proc    processIface
		alive   bool
		pending bool
		noop    bool
		err     error
	)
	r.ss.Update(func(tx sessTx) {
		sess := tx.Get(key)
		if sess == nil {
			pending = true
			if !setPick(tx.Ext().picks.accessProfile, key, profile) {
				err = ErrTuningCapacity
				return
			}
			if model != nil {
				err = r.setPendingTuning(tx, key, model, nil)
			}
			return
		}
		if aws != "" {
			if _, backendID := r.backends.wrapperFor(sess.Backend()); backendID != "claude" {
				err = fmt.Errorf("access profile %q carries its own AWS credentials, which only the claude backend reads, not %q", profile, backendID)
				return
			}
		}
		if sess.State() == "running" {
			err = ErrAccessProfileSwitchBusy
			return
		}
		modelChanged := model != nil && *model != sess.TuningModel()
		if profile == sess.AccessProfile() && !modelChanged {
			noop = true
			return
		}
		sess.SetAccessProfile(profile)
		if modelChanged {
			sess.SetTuningModel(*model)
		}
		proc = sess.loadProcess()
		alive = proc != nil && proc.Alive()
		if alive {
			storeAtomicString(&sess.deathReason, "access_profile_switch")
		}
		tx.MarkChanged()
	})
	switch {
	case err != nil:
		return "", err
	case pending, noop:
		return TuningAppliedDeferred, nil
	}

	logKey := osutil.SanitizeForLog(key, 64)
	if alive {
		proc.Close()
		// Same bookkeeping as a tuning respawn: recount the slot and wake
		// Shutdown's wait in one transaction.
		r.ss.Update(func(tx sessTx) {
			tx.Broadcast()
			r.countActive(tx)
		})
	}
	r.notifyChange()
	slog.InfoContext(ctx, "session access profile switched", "key", logKey,
		"access_profile", profile, "model_set", model != nil, "respawn", alive)
	if alive {
		return TuningAppliedRespawn, nil
	}
	return TuningAppliedDeferred, nil
}
