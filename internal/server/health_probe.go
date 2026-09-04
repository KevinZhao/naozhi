package server

import (
	"time"

	"github.com/naozhi/naozhi/internal/session"
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

// subsystemProbes returns the HealthProbe closures the authenticated /health
// handler fans out over. Each probe writes a distinct field, so order does
// not affect the JSON; every probe is nil-safe for harnesses without a router.
func (h *HealthHandler) subsystemProbes() []HealthProbe {
	return []HealthProbe{
		wsDroppedHealthProbe(h.hubDropped),
		dispatchHealthProbe(h.dispatcherMetrics),
		EventLogHealthProbe(h.router),
		AttachmentTrackerHealthProbe(h.router),
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
// sub-object from the injected dispatcherMetrics closure. Last-reply fields
// are emitted only once a reply has succeeded; nil closure omits the object.
func dispatchHealthProbe(metrics func() (int64, int64, int64, time.Time)) HealthProbe {
	return func(auth *healthAuthSection) {
		if auth == nil || metrics == nil {
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
