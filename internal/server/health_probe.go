package server

import (
	"github.com/naozhi/naozhi/internal/session"
)

// HealthProbe populates one or more /health auth-section fields without
// requiring handleHealth to fan out manually. Each probe is a closure
// that mutates the in-progress *healthAuthSection — typed fields stay
// typed, but the per-subsystem source-of-truth (eventlog,
// attachment_tracker, …) lives next to its owner instead of being
// inlined in one giant handler. R247-ARCH-12 (#647).
//
// Scope: the type plus the default eventlog / attachment-tracker probe
// factories. handleHealth now fans out over subsystemProbes() instead of
// inlining the per-subsystem field copy (#1052) — the factories are the
// single source of truth for each subsystem's wire mapping. The
// migration kept the JSON output byte-identical to the prior inline form
// (covered by the health-shape regression tests).
//
// Wire-shape contract: probes MUST keep the JSON output byte-identical
// to the prior inline form so existing dashboard / monitoring callers
// see no change. Disabled subsystems leave their nullable pointer field
// nil so the section is omitted via omitempty (same shape as before).
//
// Today only the eventlog and attachment-tracker sub-sections route
// through this interface — they were the two cleanest fits because
// each already has a session.<X>Stats wire struct mapped 1:1 to a
// healthAuthSection field. Top-level fields (sessions / goroutines /
// system / nodes / platforms / dispatch / watchdog) remain inline
// because they touch many fields at once or read HealthHandler-private
// state directly; future RFC work can migrate them when each subsystem
// owns its corresponding wire struct.
type HealthProbe func(auth *healthAuthSection)

// EventLogHealthProbe returns a HealthProbe that populates the
// eventlog auth-section field from the router-attached EventLog
// subsystem. Returned as a closure so callers can register from any
// wiring point that holds a *session.Router. The returned probe is a
// no-op when EventLog is disabled (omitempty keeps the section out
// of the JSON response). R247-ARCH-12 (#647).
//
// Naming: exported so a future server.New / Server.Start integration
// can wire it without forcing the wiring code to live in the same
// file as the probe definition. Internal-only callers can still use
// it (Go does not gate cross-file visibility within a package).
func EventLogHealthProbe(router *session.Router) HealthProbe {
	return func(auth *healthAuthSection) {
		if router == nil || auth == nil {
			return
		}
		el := router.EventLogStats()
		if !el.Enabled {
			return
		}
		auth.EventLog = &healthEventLogStats{
			Dir:            el.Dir,
			WriterAlive:    el.WriterAlive,
			ChannelDepth:   el.ChannelDepth,
			ChannelCap:     el.ChannelCap,
			LastDrainMsAgo: el.LastDrainMsAgo,
			Written:        el.Written,
			Dropped:        el.Dropped,
			Fsyncs:         el.Fsyncs,
			Malformed:      el.Malformed,
			ReplayLeak:     el.ReplayLeak,
			FSType:         el.FSType,
			FSSupported:    el.FSSupported,
		}
	}
}

// subsystemProbes returns the ordered list of HealthProbe closures the
// authenticated /health handler fans out over to populate per-subsystem
// auth-section fields. Centralising the registration here (next to the
// factories) means adding a new subsystem probe is a one-line edit in
// this file rather than another inline block in handleHealth. The order
// is irrelevant to the JSON output (each probe writes a distinct field)
// but is kept stable (eventlog, then attachment-tracker) to match the
// prior inline ordering for anyone diffing the handler. R247-ARCH-12
// (#1052).
//
// Nil-safe: each factory's returned closure no-ops when its router is
// nil or the subsystem is disabled, so a HealthHandler built by a test
// harness without a live router fans out harmlessly.
func (h *HealthHandler) subsystemProbes() []HealthProbe {
	return []HealthProbe{
		EventLogHealthProbe(h.router),
		AttachmentTrackerHealthProbe(h.router),
	}
}

// AttachmentTrackerHealthProbe is the analogous factory for the
// router-attached AttachmentTracker subsystem. Same shape and
// disabled-as-noop semantics as EventLogHealthProbe.
// R247-ARCH-12 (#647).
func AttachmentTrackerHealthProbe(router *session.Router) HealthProbe {
	return func(auth *healthAuthSection) {
		if router == nil || auth == nil {
			return
		}
		at := router.AttachmentTrackerStats()
		if !at.Enabled {
			return
		}
		auth.AttachmentTracker = &healthAttachTrackStats{
			WriterAlive:  at.WriterAlive,
			ChannelDepth: at.ChannelDepth,
			ChannelCap:   at.ChannelCap,
			LastDrainMs:  at.LastDrainMs,
			Pending:      at.Pending,
			Written:      at.Written,
			Cleared:      at.Cleared,
			Dropped:      at.Dropped,
			Errors:       at.Errors,
		}
	}
}
