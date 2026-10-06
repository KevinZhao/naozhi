package sessionview

import "errors"

// Tuning apply modes SetSessionTuning reports (dashboard `applied_via`).
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

// ErrTuningEffortUnsupported is returned when an effort tier is set for a
// session whose backend protocol does not honour SpawnOptions.Effort.
// Silently recording it would let the operator believe a tier is in force
// when it is not.
var ErrTuningEffortUnsupported = errors.New("backend does not support effort tiers")
