package runtelemetry

// Broadcaster is the consumer-side interface that each scheduler
// (cron / sysession / future planner) registers exactly once with its
// host (typically *server.Hub). Producers invoke Broadcast{Started,Ended}
// from outside any internal lock; implementations MUST NOT call back into
// the producer (no router state lookups, no Scheduler method calls).
//
// Implementations are responsible for:
//   - selecting the per-Subsystem WS payload shape (cron_run_* vs
//     daemon_run_*),
//   - selecting the per-Subsystem OwnerID sanitiser (see
//     RunStartedEvent.OwnerID godoc),
//   - deciding whether to include RunEndedEvent.ErrorMsg on the wire
//     (see RunEndedEvent godoc — SECURITY note).
//
// A nil Broadcaster registration is legal and means "no broadcast" —
// useful for tests and no-WS deployments. Producers MUST nil-check
// before invoke.
type Broadcaster interface {
	BroadcastRunStarted(ev RunStartedEvent)
	BroadcastRunEnded(ev RunEndedEvent)
}
