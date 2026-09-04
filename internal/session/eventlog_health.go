package session

import (
	"time"

	"github.com/naozhi/naozhi/internal/eventlog/persist"
)

// EventLogHealth is the /health.eventlog sub-object shape. Callers
// must not mutate the returned value; the Router re-synthesises it
// on every call so the /health handler can render it without
// holding any lock.
//
// Enabled=false means the Router was constructed with EventLogDir
// empty and no persister exists — in that case every other field
// is zero-valued and /health should omit the entire sub-object.
type EventLogHealth struct {
	Enabled        bool
	Dir            string
	WriterAlive    bool
	ChannelDepth   int
	ChannelCap     int
	LastDrainMsAgo int64
	Written        int64
	Dropped        int64
	Fsyncs         int64
	Malformed      int64
	ReplayLeak     int64

	// FSType / FSSupported mirror the Persister's cached filesystem
	// detection (RFC §5.4). FSSupported==false is the signal that
	// doctor / dashboard banner should render a warning.
	FSType      string
	FSSupported bool
}

// EventLogStats returns a snapshot of the persister's observability state;
// EventLogHealth{Enabled:false} when disabled. Lives here so /health (server
// package) need not import persist directly.
func (r *Router) EventLogStats() EventLogHealth {
	if r == nil || r.eventLogPersister == nil {
		return EventLogHealth{}
	}
	s := r.eventLogPersister.Stats()
	var lastMs int64
	if s.LastDrainAgo > 0 {
		lastMs = s.LastDrainAgo.Milliseconds()
	}
	return EventLogHealth{
		Enabled:        true,
		Dir:            r.eventLogDir,
		WriterAlive:    r.eventLogPersister.WriterAlive(),
		ChannelDepth:   s.ChannelDepth,
		ChannelCap:     s.ChannelCap,
		LastDrainMsAgo: lastMs,
		Written:        s.Written,
		Dropped:        s.Dropped,
		Fsyncs:         s.Fsyncs,
		Malformed:      s.Malformed,
		ReplayLeak:     s.ReplayLeak,
		FSType:         s.FSType,
		FSSupported:    s.FSSupported,
	}
}

// Keeps the persist import live (only its Stats struct is used here).
var _ = persist.Stats{}

// EventLogWriterHealthy returns a single boolean suitable for a monitor rule.
func (r *Router) EventLogWriterHealthy() bool {
	s := r.EventLogStats()
	return !s.Enabled || s.WriterAlive
}

// Keeps the `time` import live.
var _ = time.Nanosecond

// AttachmentTrackerHealth is the /health.attachment_tracker sub-object shape.
// Callers must not mutate; the Router re-builds it on every request. See
// docs/rfc/attachment-refcount.md §3.2.
type AttachmentTrackerHealth struct {
	Enabled      bool
	WriterAlive  bool
	ChannelDepth int
	ChannelCap   int
	LastDrainMs  int64
	Written      int64
	Cleared      int64
	Dropped      int64
	Errors       int64
	Pending      int
}

// AttachmentTrackerStats mirrors EventLogStats for the tracker. Returns
// Enabled=false when no tracker was constructed (eventLogDir="").
func (r *Router) AttachmentTrackerStats() AttachmentTrackerHealth {
	if r == nil || r.attachmentTracker == nil {
		return AttachmentTrackerHealth{}
	}
	s := r.attachmentTracker.Stats()
	return AttachmentTrackerHealth{
		Enabled:      true,
		WriterAlive:  r.attachmentTracker.WriterAlive(),
		ChannelDepth: s.ChannelDepth,
		ChannelCap:   s.ChannelCap,
		LastDrainMs:  s.LastDrainMs,
		Written:      s.Written,
		Cleared:      s.Cleared,
		Dropped:      s.Dropped,
		Errors:       s.Errors,
		Pending:      s.Pending,
	}
}
