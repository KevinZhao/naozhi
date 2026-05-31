package session

// ManagedState is the explicit lifecycle state of a ManagedSession.
//
// R176-ARCH-N4 (#432): historically the four/five logical states were
// reconstructed ad-hoc at every consumer (dashboard, cron, discovery) by
// stitching together `loadProcess() == nil`, `isAlive()`, `getSessionID()`
// and the `exempt` flag. That field-stitching duplicated the inference rules
// and let them drift. ManagedState collapses the inference into a single
// derived accessor (ManagedSession.ManagedState) so callers read one value
// instead of re-deriving the matrix.
//
// The state is DERIVED on read from the existing atomic fields — it is NOT a
// new persisted field, so there is no store-format bump and no migration. The
// existing String()-based State() ("ready"/"busy"/…) accessor keeps reporting
// the live *process* state for the high-frequency connector push path; this
// enum answers the orthogonal "where is this session in its lifecycle"
// question that the dashboard and catalog views actually need.
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
// Single source of truth for the inference that consumers previously
// open-coded (R176-ARCH-N4 / #432). Lock-free: reads only atomic fields plus
// the immutable `exempt` flag.
//
// Precedence:
//  1. exempt wins (planner/scratch are badged distinctly regardless of proc),
//  2. a live process → alive,
//  3. no live process but a captured session ID → suspended (resumable),
//  4. no live process and no session ID → stub if never spawned, else dead.
//
// "never spawned" is approximated by "no session ID AND no persisted history":
// a session that once ran always either captured a session ID or accumulated
// event history, so the absence of both is the stub signature.
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
