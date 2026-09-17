package cron

import (
	"log/slog"
	"runtime/debug"
)

// goStartupPass runs one cold-start reconciliation pass in its own goroutine,
// tracked by gcWG so Stop waits for it, and isolated by a recover.
//
// The recover is the point, and it was missing from all three passes. These run
// from Start with nothing above them on the stack — robfig/cron's Recover wrapper
// covers scheduled ticks, not this — so a panic in one takes the whole process
// down before the dashboard is ever served. That is worse than it sounds because
// a startup panic is the kind that repeats: the supervisor restarts naozhi, the
// same on-disk state is read again, and the crash loop has no upper bound of its
// own. Skipping one reconciliation pass costs some history rows; crash-looping
// costs the operator every session on the host.
//
// gcWG.Add happens on the caller's goroutine, before the pass starts, so a Stop
// racing Start cannot Wait past a pass that has not been counted yet.
func (s *Scheduler) goStartupPass(name string, fn func()) {
	s.gcWG.Add(1)
	go func() {
		defer s.gcWG.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("cron: cold-start pass panicked; that pass is skipped, the scheduler keeps running",
					"pass", name,
					"panic", r,
					"stack", string(debug.Stack()))
			}
		}()
		fn()
	}()
}
