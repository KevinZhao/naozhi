package server

import (
	"time"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/spawndiag"
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
// subsystem. No-op when the router is nil or EventLog is disabled.
func EventLogHealthProbe(router *session.Router) HealthProbe {
	return func(auth *healthAuthSection) {
		if router == nil || auth == nil {
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
			FSType:         el.FSType,
			FSSupported:    el.FSSupported,
		}
	}
}

// subsystemProbes returns the HealthProbe closures the authenticated /health
// handler fans out over. Each probe writes a distinct field, so order does
// not affect the JSON; every probe is nil-safe for harnesses without a router.
func (h *HealthHandler) subsystemProbes() []HealthProbe {
	return []HealthProbe{
		platformConnProbe(h.platforms),
		wsDroppedHealthProbe(h.hubDropped),
		dispatchHealthProbe(h.dispatcherMetrics),
		EventLogHealthProbe(h.router),
		AttachmentTrackerHealthProbe(h.router),
		runStoresHealthProbe(h.cronRunStore, h.router),
		sessionStoreHealthProbe(h.router),
		spawnDiagsHealthProbe,
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
		Since:     cs.Since.UTC().Format(time.RFC3339),
		SinceAgo:  now.Sub(cs.Since).Round(time.Second).String(),
		LastError: cs.LastError,
	}
	if !cs.LastErrorAt.IsZero() {
		out.LastErrorAt = cs.LastErrorAt.UTC().Format(time.RFC3339)
	}
	return out
}

// sessionStoreHealthProbe populates session_store with the store files whose
// writes are currently refused. The section is omitted while there are none:
// the router's saves are the only thing that lifts or sets a block, so the
// field is exactly "is session state reaching disk right now".
func sessionStoreHealthProbe(router *session.Router) HealthProbe {
	return func(auth *healthAuthSection) {
		if router == nil || auth == nil {
			return
		}
		blocked := router.StoreWriteBlocks()
		if len(blocked) == 0 {
			return
		}
		auth.SessionStore = &healthSessionStore{Blocked: blocked}
	}
}

// spawnDiagsHealthProbe populates spawn_diags from the process-wide gate
// summary; the field stays omitted until some gate has dropped an input.
func spawnDiagsHealthProbe(auth *healthAuthSection) {
	if s, ok := spawndiag.Snapshot(); ok {
		auth.SpawnDiags = &s
	}
}

// runStoresHealthProbe populates run_stores from the cron and session
// run-history stores. A store that does not persist contributes no
// sub-object, and with neither persisting the section is omitted.
func runStoresHealthProbe(cronRunStore func() cron.RunStoreHealth, router *session.Router) HealthProbe {
	return func(auth *healthAuthSection) {
		if auth == nil {
			return
		}
		var rs healthRunStores
		if cronRunStore != nil {
			if c := cronRunStore(); c.Enabled {
				rs.Cron = &healthCronRunStore{
					WriteFailedDiskFull: c.WriteFailedDiskFull,
					WriteFailedOther:    c.WriteFailedOther,
					HistoryDropped:      c.HistoryDropped,
					CacheStaleEvictions: c.CacheStaleEvictions,
				}
			}
		}
		if router != nil {
			if sr := router.Runs().Health(); sr.Enabled {
				rs.Session = &healthSessionRunStore{
					WriteFailedDiskFull: sr.WriteFailedDiskFull,
					WriteFailedOther:    sr.WriteFailedOther,
					AsyncDropped:        sr.AsyncDropped,
				}
			}
		}
		if rs.Cron == nil && rs.Session == nil {
			return
		}
		auth.RunStores = &rs
	}
}

// wsDroppedHealthProbe returns a HealthProbe that populates the ws_dropped
// field from the hub's DroppedMessages counter. Injected as a closure so
// HealthHandler has no upward dependency on the Hub; nil closure omits the field.
func wsDroppedHealthProbe(hubDropped func() int64) HealthProbe {
	return func(auth *healthAuthSection) {
		if auth == nil || hubDropped == nil {
			return
		}
		n := hubDropped()
		auth.WSDropped = &n
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
func AttachmentTrackerHealthProbe(router *session.Router) HealthProbe {
	return func(auth *healthAuthSection) {
		if router == nil || auth == nil {
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
