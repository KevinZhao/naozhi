package dispatch

import (
	"context"
	"time"
)

// detachedNotifyCtx returns a fresh context.Background-rooted ctx with the
// supplied timeout, intentionally severing inheritance from parent. Used by
// dispatch outbound paths that MUST deliver a user-visible notification even
// when the surrounding turn / handler ctx is already Done (shutdown,
// upstream cancel, panic recovery).
//
// R247-ARCH-10 (#632, cluster R244-ARCH-2 / R246-ARCH-17): four sites in
// dispatch.go previously open-coded `context.WithTimeout(context.Background(),
// X)` with the same "reply must not be cancelled by parent" intent. The
// detach-from-parent rule lives here exactly once so future sites can't
// silently drop the rule by re-using the wrong ctx; reviewers see a single
// audited factory rather than a recurring open-coded pattern.
//
// The returned cancel MUST be deferred by the caller — leaking the
// timer goroutine on every panic-recovery / shutdown notify would
// accumulate into a slow leak under sustained restart loops.
//
// Note: the cron package's analogous site (scheduler_notify.go) is not
// migrated here yet — it would require a circular import (cron already
// imports dispatch via its slash-command surface). That site is tracked
// as the remaining hold-out under #632.
func detachedNotifyCtx(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}
