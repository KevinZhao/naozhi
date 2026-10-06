package runtelemetry

// Tee returns a Broadcaster that forwards every event to a then b, so a
// second consumer (an outbound webhook sender) can sit beside the dashboard
// Hub without the producers knowing. A nil side is skipped; both nil yields
// nil, which producers already treat as "no broadcast".
func Tee(a, b Broadcaster) Broadcaster {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	return tee{a, b}
}

type tee struct{ a, b Broadcaster }

func (t tee) BroadcastRunStarted(ev RunStartedEvent) {
	t.a.BroadcastRunStarted(ev)
	t.b.BroadcastRunStarted(ev)
}

func (t tee) BroadcastRunEnded(ev RunEndedEvent) {
	t.a.BroadcastRunEnded(ev)
	t.b.BroadcastRunEnded(ev)
}
