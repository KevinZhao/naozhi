package session

import (
	"log/slog"
	"time"
)

// ReleaseIdleProcess closes an exempt session's idle CLI process and keeps the
// session, so the next GetOrCreate resumes it under the same session id. It
// refuses a non-exempt session, one without a live process and one whose turn
// is still running (or whose Send holds sendMu); true means a process was
// closed. The bounded socket wait keeps an immediate re-spawn of the key off
// the shim's dial-first "refusing to clobber" guard, as releaseKeys does.
func (s *ManagedSession) ReleaseIdleProcess() bool {
	if !s.exempt || !s.sendMu.TryLock() {
		return false
	}
	proc := s.loadProcess()
	if proc == nil || !proc.Alive() || proc.IsRunning() {
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
