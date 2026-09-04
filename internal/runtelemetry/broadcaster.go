package runtelemetry

// Broadcaster is the consumer-side interface each scheduler (cron /
// sysession) registers exactly once with its host (typically *server.Hub).
// Producers call Broadcast{Started,Ended} outside any internal lock;
// implementations MUST NOT call back into the producer. They pick the
// per-Subsystem WS payload shape and OwnerID sanitiser, and decide whether
// RunEndedEvent.ErrorMsg goes on the wire (see its SECURITY note).
// A nil registration means "no broadcast"; producers MUST nil-check.
type Broadcaster interface {
	BroadcastRunStarted(ev RunStartedEvent)
	BroadcastRunEnded(ev RunEndedEvent)
}
