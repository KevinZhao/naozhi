// classify.go is the ONLY file in internal/usermsg that imports the
// implementation packages (cli + session). It maps their sentinel errors
// onto a presentation-neutral Code enum; usermsg.go maps Code → Chinese text
// with zero cli/session dependency, so the text tables can move to
// internal/i18n (#631) untouched.
package usermsg

import (
	"context"
	"errors"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// Code is a stable, package-neutral classification of a send-path error.
// Values are NOT persisted or sent over the wire, so they may be reordered.
type Code int

const (
	// CodeUnknown is the fall-through for errors with no dedicated branch.
	CodeUnknown Code = iota
	CodeMaxProcs
	CodeMaxExemptSessions
	CodeNoCLIWrapper
	CodeSessionAsleep
	CodeCronAsleep
	CodeTimeout
	CodeProcessExited
	CodeAbortedByUrgent
	CodeReconnectedUnknown
	CodeSessionReset
	CodeTooManyPending
	CodeProcessBusy
	CodeMessageTooLarge
	CodeRestarting
)

// classify maps err onto a Code using errors.Is so wrapped sentinels still
// match. A cron-namespace key turns the asleep case into CodeCronAsleep.
// Callers short-circuit nil err before calling.
func classify(err error, key string) Code {
	switch {
	case errors.Is(err, session.ErrMaxProcs):
		return CodeMaxProcs
	case errors.Is(err, session.ErrMaxExemptSessions):
		return CodeMaxExemptSessions
	case errors.Is(err, session.ErrNoCLIWrapper):
		return CodeNoCLIWrapper
	case errors.Is(err, session.ErrNoActiveProcess):
		if sessionkey.IsCronKey(key) {
			return CodeCronAsleep
		}
		return CodeSessionAsleep
	case errors.Is(err, cli.ErrNoOutputTimeout), errors.Is(err, cli.ErrTotalTimeout):
		return CodeTimeout
	case errors.Is(err, cli.ErrProcessExited):
		return CodeProcessExited
	case errors.Is(err, cli.ErrAbortedByUrgent):
		return CodeAbortedByUrgent
	case errors.Is(err, cli.ErrReconnectedUnknown):
		return CodeReconnectedUnknown
	case errors.Is(err, cli.ErrSessionReset):
		return CodeSessionReset
	case errors.Is(err, cli.ErrTooManyPending):
		return CodeTooManyPending
	case errors.Is(err, cli.ErrProcessBusy):
		return CodeProcessBusy
	case errors.Is(err, cli.ErrMessageTooLarge):
		return CodeMessageTooLarge
	case errors.Is(err, cli.ErrOrphanedSlot):
		// Orphaned slot surfaces to the user as a plain timeout retry hint.
		return CodeTimeout
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return CodeRestarting
	case errors.Is(err, session.ErrRouterStopped):
		// Shutdown in progress: a "/new reset" cannot succeed either, so surface
		// the restarting hint instead of the misleading CodeUnknown advice.
		return CodeRestarting
	default:
		return CodeUnknown
	}
}

// isNoOutputTimeout / isTotalTimeout are the timeout sentinels UserMessage
// specialises with a concrete duration; kept here next to classify.
func isNoOutputTimeout(err error) bool { return errors.Is(err, cli.ErrNoOutputTimeout) }
func isTotalTimeout(err error) bool    { return errors.Is(err, cli.ErrTotalTimeout) }
