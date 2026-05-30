package session

import (
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// loadProcess returns the currently attached processIface, or nil when
// the session is detached (paused, reclaimed, or never spawned).
//
// Implementation note: s.process is an atomic.Pointer[processBox] — we
// wrap the iface in a one-field struct because Go's atomic.Pointer is
// generic over a concrete type and requires non-nil iface assertions to
// store directly. The "load box, dereference" indirection is the cost
// of getting lock-free read/write semantics for an interface value.
// Callers that only need liveness should prefer isAlive() over
// loadProcess() != nil to also catch dead-but-attached processes.
func (s *ManagedSession) loadProcess() processIface {
	if box := s.process.Load(); box != nil {
		return box.p
	}
	return nil
}

// storeProcess atomically replaces the attached process. Passing nil
// detaches; passing a non-nil iface re-wraps in a fresh processBox so
// concurrent loadProcess callers see a consistent (box, p) pair without
// torn reads. Must be paired with sendMu / spawnMu by the caller — this
// function only handles the atomic publication, not the lifecycle
// invariant that only one process is attached at a time.
func (s *ManagedSession) storeProcess(p processIface) {
	if p == nil {
		s.process.Store(nil)
	} else {
		s.process.Store(&processBox{p: p})
	}
}

// isAlive returns true only when a process is attached AND its Alive()
// reports the underlying handle has not exited. Lock-free; uses
// loadProcess() so it is safe to call from any goroutine. The dual
// nil + Alive check is required because the readLoop transitions
// process state to dead before storeProcess(nil) detaches.
func (s *ManagedSession) isAlive() bool {
	p := s.loadProcess()
	return p != nil && p.Alive()
}

// ReattachProcess safely injects a reconnected shim process into this session.
// Called by Router.reconnectShims after naozhi restart.
func (s *ManagedSession) ReattachProcess(proc processIface, sessionID string) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	snapshot := s.attachProcessAndSnapshotPersisted(proc)
	s.setSessionID(sessionID)
	storeAtomicString(&s.deathReason, "")
	s.lastActive.Store(time.Now().UnixNano())

	// attachProcessAndSnapshotPersisted returns nil snapshot when proc is nil,
	// so len(snapshot) > 0 already implies proc != nil. R231-CQ-3.
	if len(snapshot) > 0 {
		proc.InjectHistory(snapshot)
	}

	if s.onSessionID != nil && sessionID != "" {
		s.onSessionID(sessionID)
	}
}

// ReattachProcessNoCallback is like ReattachProcess but skips the onSessionID
// callback. Used when the caller already holds router.mu and will track the
// session ID directly (avoids deadlock since onSessionID acquires router.mu).
//
// Does NOT acquire sendMu: all operations here are atomic stores, and the
// caller already holds router.mu (write). Acquiring sendMu here would violate
// the documented lock ordering (sendMu → router.mu) and risk ABBA deadlock
// with Send() which holds sendMu then calls onSessionID → router.mu.
//
// SAFETY CONSTRAINT: this function must only be called when Send() cannot be
// in flight for this session. Two callers satisfy this:
//   - ReconnectShims at startup, before any Send can have begun.
//   - The runtime reconcile loop (router_shim.go), which acquires sess.sendMu
//     via TryLock BEFORE r.mu and holds it across this call — see #750. If
//     that TryLock fails a Send is in flight and the session is skipped.
//
// If Send() were concurrently executing, the deathReason.Store("") here could
// silently erase a diagnostic death reason that Send() just set, and the
// process pointer swap would publish a fresh process from under the in-flight
// Send. The lack of an internal sendMu acquisition makes that a logical race
// even though each individual Store is atomic — hence the caller-side contract
// above.
func (s *ManagedSession) ReattachProcessNoCallback(proc processIface, sessionID string) {
	snapshot := s.attachProcessAndSnapshotPersisted(proc)
	s.setSessionID(sessionID)
	storeAtomicString(&s.deathReason, "")
	s.lastActive.Store(time.Now().UnixNano())
	// attachProcessAndSnapshotPersisted returns nil snapshot when proc is nil,
	// so len(snapshot) > 0 already implies proc != nil. R231-CQ-3.
	if len(snapshot) > 0 {
		proc.InjectHistory(snapshot)
	}
}

// adoptProcessAlreadySeeded publishes proc and marks the entire current
// persistedHistory as already-seeded into proc.EventLog. Used by Rename /
// takeover paths where the proc was running under a different ManagedSession
// and already has the matching entries in its ring; we must NOT re-inject
// (would duplicate every bubble) but we DO need persistedSeededLen aligned
// so the next InjectHistory tail still forwards.
//
// R231-CQ-5: the verb pair "adopt … AlreadySeeded" vs
// "attach … AndSnapshotPersisted" intentionally diverges to encode the two
// distinct semantics — adopt = "treat persistedHistory as if proc has it
// already, do not return a snapshot"; attach = "publish proc + return the
// persistedHistory slice so the caller can re-seed". A blanket rename to
// match the styles would lose that signal at the call site, so this godoc
// pins the contrast instead. See attachProcessAndSnapshotPersisted's godoc
// for the symmetric path.
func (s *ManagedSession) adoptProcessAlreadySeeded(proc processIface) {
	s.historyMu.Lock()
	s.storeProcess(proc)
	s.persistedSeededLen = len(s.persistedHistory)
	s.historyMu.Unlock()
}

// attachProcessAndSnapshotPersisted publishes proc as the session's live
// process and atomically snapshots the persistedHistory prefix that the new
// proc still needs to be seeded with. The two writes share historyMu so
// concurrent InjectHistory calls observe a consistent (process, seededLen)
// pair: an InjectHistory that wins the lock first sees seededLen=0 and the
// old (likely nil) process, appends to persistedHistory, and forwards to the
// fresh process if one is already attached; an InjectHistory that loses the
// race sees seededLen == len(persistedHistory) so its forwarding loop only
// pushes the truly new tail (no double-injection).
//
// Returns a defensive copy because proc.InjectHistory consumes the slice and
// runs after we release historyMu — handing it the live persistedHistory
// backing array would race with subsequent appends.
//
// R231-CQ-5: the verb pair "attach … AndSnapshotPersisted" vs
// "adopt … AlreadySeeded" intentionally diverges — attach returns the
// persistedHistory slice so the caller re-seeds; adopt treats the slice as
// already in proc.EventLog and returns nothing. See adoptProcessAlreadySeeded
// for the symmetric path.
func (s *ManagedSession) attachProcessAndSnapshotPersisted(proc processIface) []cli.EventEntry {
	s.historyMu.Lock()
	if proc == nil {
		// R231-CQ-2: nil parameter = "session is now process-less" (detach
		// path used by ResetAndRecreate / Cleanup / Remove). The decision to
		// also reset persistedSeededLen=0 is deliberate: when a fresh process
		// later attaches via this same function, it MUST be re-seeded with
		// the full persistedHistory snapshot, otherwise the dashboard
		// renders empty until new live events arrive. Leaving seededLen at
		// its prior non-zero value would make the next attach skip the
		// snapshot and the new proc's EventLog would start blank against
		// the persisted history that the session still remembers.
		//
		// persistedHistory itself is NOT cleared — the chat key + workspace
		// stay the same across the detach/reattach pair, so its content is
		// still valid. Only the "what proc has been seeded with" pointer
		// resets. Mirrors adoptProcessAlreadySeeded's symmetric contract:
		// adopt = "proc already has the events"; attach(nil) = "no proc,
		// next attach must re-seed from scratch".
		s.storeProcess(nil)
		s.persistedSeededLen = 0
		s.historyMu.Unlock()
		return nil
	}
	s.storeProcess(proc)
	n := len(s.persistedHistory)
	var snapshot []cli.EventEntry
	if n > 0 {
		snapshot = make([]cli.EventEntry, n)
		copy(snapshot, s.persistedHistory)
	}
	s.persistedSeededLen = n
	s.historyMu.Unlock()
	return snapshot
}

// LastActive returns the last active time.
func (s *ManagedSession) LastActive() time.Time {
	return time.Unix(0, s.lastActive.Load())
}

// touchLastActive updates the last active timestamp.
func (s *ManagedSession) touchLastActive() {
	s.lastActive.Store(time.Now().UnixNano())
}

// initCreatedAtIfUnset stamps createdAt to now when it has not been set yet.
// Idempotent: a non-zero value is left alone, so Rename / loadStore paths that
// preload the original creation timestamp keep sidebar order stable.
func (s *ManagedSession) initCreatedAtIfUnset() {
	if s.createdAt.Load() == 0 {
		s.createdAt.Store(time.Now().UnixNano())
	}
}

// createdAtMillis returns the createdAt instant in unix milliseconds for the
// dashboard payload. Zero stays zero so the JSON omitempty check fires for
// sessions that somehow never received a stamp.
func (s *ManagedSession) createdAtMillis() int64 {
	v := s.createdAt.Load()
	if v == 0 {
		return 0
	}
	return v / int64(time.Millisecond)
}
