package session

// ManagedState is the explicit lifecycle state of a ManagedSession (#432),
// the single derived accessor (ManagedSession.ManagedState) consumers use
// instead of stitching `loadProcess() == nil` / `isAlive()` / `getSessionID()`
// / `exempt` together themselves.
//
// The state is DERIVED on read from the existing atomic fields — NOT a
// persisted field, so there is no store-format bump. State() ("ready"/"busy"/…)
// keeps reporting the live *process* state for the connector push path; this
// enum answers the orthogonal "where is this session in its lifecycle" question.
type ManagedState int

const (
	// StateStub is a known-but-never-spawned session: no process has ever
	// attached and no CLI session ID was captured. Cron stubs and
	// register-for-resume placeholders start here.
	StateStub ManagedState = iota
	// StateAlive has a live process attached (loadProcess()!=nil && Alive()).
	StateAlive
	// StateSuspended has no live process but retains a CLI session ID, so it
	// can be resumed (--resume) from disk history. This is the steady state
	// for an idle-evicted or restart-restored session.
	StateSuspended
	// StateDead had a process that exited without a resumable session ID —
	// it cannot be resumed and is awaiting cleanup.
	StateDead
	// StateExempt is a session excluded from TTL/eviction/activeCount
	// (planner / scratch). Reported regardless of process liveness so the
	// dashboard can badge it distinctly; callers needing the underlying
	// liveness should still consult State()/isAlive().
	StateExempt
)

// String returns a stable lowercase token for logs, metrics labels, and the
// dashboard payload. Kept distinct from State()'s process-state tokens
// ("ready"/"busy") so the two never collide in a shared field.
func (m ManagedState) String() string {
	switch m {
	case StateStub:
		return "stub"
	case StateAlive:
		return "alive"
	case StateSuspended:
		return "suspended"
	case StateDead:
		return "dead"
	case StateExempt:
		return "exempt"
	default:
		return "unknown"
	}
}

// ManagedState derives the session's lifecycle state from its current fields.
//
// Locking: NOT lock-free — the final fallback calls hasInjectedHistory(),
// which takes s.historyMu.RLock(). Callers must not hold a higher-layer lock
// (e.g. router.mu): historyMu is never held together with r.mu (router_core.go),
// and nesting here would create one half of an AB-BA deadlock.
//
// Precedence: exempt → alive (live process) → suspended (session ID captured)
// → dead (history but no session ID) → stub. "Never spawned" is approximated
// by "no session ID AND no persisted history".
func (s *ManagedSession) ManagedState() ManagedState {
	if s.exempt {
		return StateExempt
	}
	if s.isAlive() {
		return StateAlive
	}
	if s.getSessionID() != "" {
		return StateSuspended
	}
	if s.hasInjectedHistory() {
		return StateDead
	}
	return StateStub
}
