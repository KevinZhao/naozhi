package session

import (
	"log/slog"
	"time"
)

// ReleaseIdleProcess closes an exempt session's idle CLI process and keeps the
// session, so the next GetOrCreate resumes it under the same session id. It
// refuses a non-exempt session, one without a live process, and one with any
// turn outstanding: running, holding or queued on sendMu, or still pending in
// passthrough. true means a process was closed. The bounded socket wait keeps
// an immediate re-spawn of the key off the shim's dial-first "refusing to
// clobber" guard, as releaseKeys does.
func (s *ManagedSession) ReleaseIdleProcess() bool {
	if !s.exempt || !s.sendMu.TryLock() {
		return false
	}
	proc := s.loadProcess()
	if proc == nil || !proc.Alive() || s.turnOutstanding(proc) {
		s.sendMu.Unlock()
		return false
	}
	storeAtomicString(&s.deathReason, DeathReasonReleased)
	proc.Close()
	s.sendMu.Unlock()
	if !waitSocketGoneForKey(s.key, 2*time.Second) {
		slog.Warn("session release: shim socket still bound after close; the next spawn may be refused", "key", s.key)
	}
	logSessionLifecycle("released", s.key)
	return true
}

// turnOutstanding reports whether proc has a turn running, a Send holding or
// queued on sendMu, or a passthrough send still pending.
func (s *ManagedSession) turnOutstanding(proc processIface) bool {
	return proc.IsRunning() || s.turnWaiters.Load() != 0 || proc.PassthroughDepth() != 0
}
