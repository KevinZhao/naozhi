package runtelemetry

import "sync/atomic"

// Relay is a Broadcaster that forwards to one bound later. cron and sysession
// are built before the dashboard Hub their events reach, so the host hands
// them a Relay at construction and binds the Hub's broadcaster once it
// exists. Events before the bind are dropped, which is what a nil broadcaster
// did. The zero value is ready to use.
type Relay struct {
	b atomic.Pointer[Broadcaster]
}

// Bind installs the broadcaster events are forwarded to. A second Bind panics:
// the Hub is built once, and a rebind would mean two of them.
func (r *Relay) Bind(b Broadcaster) {
	if !r.b.CompareAndSwap(nil, &b) {
		panic("runtelemetry: Relay bound twice")
	}
}

// BroadcastRunStarted forwards ev to the bound broadcaster, if any.
func (r *Relay) BroadcastRunStarted(ev RunStartedEvent) {
	if b := r.b.Load(); b != nil {
		(*b).BroadcastRunStarted(ev)
	}
}

// BroadcastRunEnded forwards ev to the bound broadcaster, if any.
func (r *Relay) BroadcastRunEnded(ev RunEndedEvent) {
	if b := r.b.Load(); b != nil {
		(*b).BroadcastRunEnded(ev)
	}
}
