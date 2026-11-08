package session

import (
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// loadProcess returns the currently attached processIface, or nil when
// the session is detached (paused, reclaimed, or never spawned).
//
// s.process is an atomic.Pointer[processBox]: the iface is boxed in a
// one-field struct because atomic.Pointer needs a concrete type, giving
// lock-free reads/writes of an interface value. Callers that only need
// liveness should prefer isAlive(), which also catches dead-but-attached.
func (s *ManagedSession) loadProcess() processIface {
	if box := s.process.Load(); box != nil {
		return box.p
	}
	return nil
}

// storeProcess atomically replaces the attached process; nil detaches. A
// fresh processBox per store means loadProcess never sees a torn (box, p)
// pair. Caller must hold sendMu / spawnMu — this only handles atomic
// publication, not the one-process-attached-at-a-time invariant.
func (s *ManagedSession) storeProcess(p processIface) {
	// Drop the outgoing process's metering view so a detached session does
	// not pin it. Best-effort GC hygiene: a racing Snapshot may briefly
	// re-publish the old view; the next Snapshot replaces it (see meteringView).
	s.meteringCache.Store(nil)
	if p == nil {
		s.process.Store(nil)
	} else {
		s.process.Store(&processBox{p: p})
	}
}

// isAlive returns true only when a process is attached AND Alive(). Lock-free.
// Both checks are needed because the readLoop marks the process dead before
// storeProcess(nil) detaches it.
func (s *ManagedSession) isAlive() bool {
	p := s.loadProcess()
	return p != nil && p.Alive()
}

// ReattachProcessNoCallback injects a reconnected shim process into this
// session without invoking the onSessionID callback; for callers that already
// hold the table lock (onSessionID acquires it).
// Does NOT acquire sendMu: the lock order is sendMu → the table lock and the caller
// holds the table lock, so taking sendMu here would risk ABBA deadlock with Send()
// (sendMu → onSessionID → the table lock).
//
// SAFETY CONSTRAINT: only call when Send() cannot be in flight for this
// session (ReconnectShims at startup, or a known-dead process). Otherwise the
// deathReason.Store("") here can erase a death reason Send() just set — a
// logical race even though each Store is atomic.
func (s *ManagedSession) ReattachProcessNoCallback(proc processIface, sessionID string) {
	snapshot := s.attachProcessAndSnapshotPersisted(proc)
	// A shim learns the ID only from init/result frames, so one resumed with
	// --resume that has not run a turn since knows none; the stored ID stays,
	// or the next save would drop the session from the store.
	if sessionID != "" {
		s.setSessionID(sessionID)
	}
	storeAtomicString(&s.deathReason, "")
	// Taking the process back is not activity: the time restored from the
	// store stands, so a restart does not make every session look just used.
	s.lastActive.CompareAndSwap(0, time.Now().UnixNano())
	// len(snapshot) > 0 implies proc != nil (snapshot is nil for nil proc).
	if len(snapshot) > 0 {
		proc.InjectHistory(snapshot)
	}
}

// tryReattachProcessNoCallback is the runtime-reconcile variant of
// ReattachProcessNoCallback: it enforces the no-in-flight-Send constraint via
// a non-blocking sendMu.TryLock. The reconcile loop holds the table lock, and a Send may
// still hold sendMu while unwinding on a just-died process, so its
// timeout/death write would race the swap + deathReason reset (#750).
//
// A blocking sendMu.Lock would invert the sendMu → the table lock order and risk ABBA
// deadlock against Send → onSessionID → the table lock; TryLock cannot. On failure the
// caller skips this session and the next reconcile tick retries. Returns true
// when the reattach completed.
func (s *ManagedSession) tryReattachProcessNoCallback(proc processIface, sessionID string) bool {
	if !s.sendMu.TryLock() {
		return false
	}
	defer s.sendMu.Unlock()
	s.ReattachProcessNoCallback(proc, sessionID)
	return true
}

// adoptProcessAlreadySeeded publishes proc and marks the entire current
// persistedHistory as already-seeded into proc.EventLog. Used by Rename /
// takeover paths where proc ran under a different ManagedSession and already
// holds the matching entries: re-injecting would duplicate every bubble, but
// persistedSeededLen must be aligned so the next InjectHistory tail forwards.
// Contrast attachProcessAndSnapshotPersisted, which returns the slice for
// the caller to re-seed.
func (s *ManagedSession) adoptProcessAlreadySeeded(proc processIface) {
	s.historyMu.Lock()
	s.storeProcess(proc)
	s.persistedSeededLen = len(s.persistedHistory)
	s.historyMu.Unlock()
}

// attachProcessAndSnapshotPersisted publishes proc as the session's live
// process and snapshots the persistedHistory prefix the new proc must be
// seeded with. Both writes happen under historyMu so a concurrent
// InjectHistory sees a consistent (process, seededLen) pair: one that loses
// the race sees seededLen == len(persistedHistory) and forwards only the new
// tail (no double-injection).
//
// Returns a defensive copy: proc.InjectHistory consumes the slice after
// historyMu is released, so handing it the live backing array would race
// with subsequent appends.
func (s *ManagedSession) attachProcessAndSnapshotPersisted(proc processIface) []clievent.EventEntry {
	s.historyMu.Lock()
	if proc == nil {
		// Detach (ResetAndRecreate / Cleanup / Remove). persistedSeededLen MUST
		// reset to 0 so the next attach re-seeds the full snapshot, otherwise
		// the new proc's EventLog starts blank. persistedHistory itself stays:
		// chat key + workspace are unchanged across detach/reattach.
		s.storeProcess(nil)
		s.persistedSeededLen = 0
		s.historyMu.Unlock()
		return nil
	}
	s.storeProcess(proc)
	n := len(s.persistedHistory)
	var snapshot []clievent.EventEntry
	if n > 0 {
		snapshot = make([]clievent.EventEntry, n)
		copy(snapshot, s.persistedHistory)
	}
	s.persistedSeededLen = n
	s.historyMu.Unlock()
	return snapshot
}

// lastActiveStep is how far the process's latest live event must be past
// LastActive to move it: a turn running for hours shows its progress in the
// session list, index.json and the Planner's snapshot within a minute, and
// what keys on the value (the search reply cache) changes at most once a
// minute rather than on every event.
const lastActiveStep = time.Minute

// LastActive returns the last active time: a turn's start or end, or the
// process's latest live event, at a minute's step.
func (s *ManagedSession) LastActive() time.Time {
	turn := s.lastActive.Load()
	return time.Unix(0, max(turn, s.catchUpLastOutput(turn)))
}

// LastTurnAt is when a turn last started or ended (or the session was
// touched, restored or taken over), without the events between: custody
// names one failure by it, and a background task's events after a failed
// turn must not make that failure a new one. Zero when never set.
func (s *ManagedSession) LastTurnAt() time.Time {
	if ns := s.lastActive.Load(); ns != 0 {
		return time.Unix(0, ns)
	}
	return time.Time{}
}

// catchUpLastOutput moves lastOutput to the process's latest live event when
// that is lastActiveStep or more past what LastActive shows, and returns it.
// Live events only: a reattached shim's replay and injected history do not
// stamp LastEventAt.
func (s *ManagedSession) catchUpLastOutput(turn int64) int64 {
	cur := s.lastOutput.Load()
	p := s.loadProcess()
	if p == nil {
		return cur
	}
	at := p.LastEventAt()
	if at.IsZero() {
		return cur
	}
	ev := at.UnixNano()
	for {
		if ev-max(turn, cur) < int64(lastActiveStep) {
			return cur
		}
		if s.lastOutput.CompareAndSwap(cur, ev) {
			return ev
		}
		cur = s.lastOutput.Load()
	}
}

// touchLastActive updates the last active timestamp: when a turn starts and
// when one ends, whoever started it. It is not the user's message: custody's
// "you are nearby" and its loop guard read the user's entries in the event
// log, which neither this nor catchUpLastOutput writes.
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
