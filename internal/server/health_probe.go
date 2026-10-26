package server

import (
	"time"

	"github.com/naozhi/naozhi/internal/platform"
)

// HealthProbe populates one or more /health auth-section fields without
// requiring handleHealth to fan out manually (#647).
//
// Wire-shape contract: a disabled subsystem MUST leave its nullable pointer
// field nil so omitempty drops the section; existing dashboard / monitoring
// callers depend on the shape.
type HealthProbe func(auth *healthAuthSection)

// EventLogHealthProbe returns a HealthProbe that populates the
// eventlog auth-section field from the router-attached EventLog
// subsystem. No-op when EventLog is disabled.
func EventLogHealthProbe(router healthRouter) HealthProbe {
	return func(auth *healthAuthSection) {
		if auth == nil {
			return
		}
		el := router.History().EventLogStats()
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
		}
	}
}

// subsystemProbes returns the HealthProbe closures the authenticated /health
// handler fans out over. Each probe writes a distinct field, so order does
// not affect the JSON; every probe is nil-safe for harnesses without a router.
func (h *HealthHandler) subsystemProbes() []HealthProbe {
	return []HealthProbe{
		platformConnProbe(h.platforms),
		dispatchHealthProbe(h.dispatcherMetrics),
		EventLogHealthProbe(h.router),
		AttachmentTrackerHealthProbe(h.router),
		sessionStoreHealthProbe(h.router),
	}
}

// platformConnProbe populates platforms and platform_conn from each adapter's
// live ConnState. platforms always carries every registered name (an empty
// object with none), falling back to "registered" for an adapter that cannot
// observe its connection; platform_conn holds only the ones that can.
func platformConnProbe(platforms map[string]platform.Platform) HealthProbe {
	return func(auth *healthAuthSection) {
		if auth == nil {
			return
		}
		auth.Platforms = make(map[string]string, len(platforms))
		for name := range platforms {
			auth.Platforms[name] = "registered"
		}
		states := platform.ConnStatesOf(platforms)
		if states == nil {
			return
		}
		now := time.Now()
		auth.PlatformConn = make(map[string]healthPlatformConn, len(states))
		for name, cs := range states {
			auth.Platforms[name] = string(cs.State)
			auth.PlatformConn[name] = healthPlatformConnOf(cs, now)
		}
	}
}

func healthPlatformConnOf(cs platform.ConnState, now time.Time) healthPlatformConn {
	out := healthPlatformConn{
		State:     string(cs.State),
		LastError: cs.LastError,
	}
	if !cs.Since.IsZero() {
		out.Since = cs.Since.UTC().Format(time.RFC3339)
		out.SinceAgo = now.Sub(cs.Since).Round(time.Second).String()
	}
	if !cs.LastErrorAt.IsZero() {
		out.LastErrorAt = cs.LastErrorAt.UTC().Format(time.RFC3339)
	}
	return out
}

// sessionStoreHealthProbe populates session_store with the store files whose
// writes are currently refused. The section is omitted while there are none.
// A block is set when the startup load finds an unreadable file and is lifted
// only by a successful load after a restart, so the field is exactly "is
// session state reaching disk right now".
func sessionStoreHealthProbe(router healthRouter) HealthProbe {
	return func(auth *healthAuthSection) {
		if auth == nil {
			return
		}
		blocked := router.StoreWriteBlocks()
		if len(blocked) == 0 {
			return
		}
		auth.SessionStore = &healthSessionStore{Blocked: blocked}
	}
}

// dispatchHealthProbe returns a HealthProbe that populates the dispatch
// sub-object from the dispatcherMetrics closure. Last-reply fields are
// emitted only once a reply has succeeded. The closure is a constructor
// argument since #2633 (the dispatcher is built before HealthHandler), so
// there is no "not wired yet" state to guard — a nil closure is a broken
// fixture and panics here rather than silently omitting the object.
func dispatchHealthProbe(metrics func() (int64, int64, int64, time.Time)) HealthProbe {
	return func(auth *healthAuthSection) {
		if auth == nil {
			return
		}
		msgs, replyErrs, sendFails, lastReply := metrics()
		d := &healthDispatchStats{
			MessageCount:    msgs,
			ReplyErrorCount: replyErrs,
			SendFailCount:   sendFails,
		}
		if !lastReply.IsZero() {
			d.LastReplySuccessAt = lastReply.UTC().Format(time.RFC3339)
			d.LastReplySuccessAgo = time.Since(lastReply).Round(time.Second).String()
		}
		auth.Dispatch = d
	}
}

// AttachmentTrackerHealthProbe is the analogous factory for the
// router-attached AttachmentTracker subsystem. Same disabled-as-noop
// semantics as EventLogHealthProbe.
func AttachmentTrackerHealthProbe(router healthRouter) HealthProbe {
	return func(auth *healthAuthSection) {
		if auth == nil {
			return
		}
		at := router.History().AttachmentTrackerStats()
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
