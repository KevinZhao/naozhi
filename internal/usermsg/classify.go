// classify.go is the ONLY file in internal/usermsg that imports the
// implementation packages (cli + session). It maps their sentinel errors
// onto a stable, presentation-neutral Code enum; usermsg.go then maps Code →
// Chinese text with zero cli/session dependency.
//
// This inverts the translation as R040034-ARCH-4 (#1413) proposes, but does
// it WITHIN usermsg so cli/session need not grow a new public SessionErrorCode
// surface yet: the errors.Is matching is confined here, and the text tables
// in usermsg.go are now decoupled from the sentinel packages. When the i18n
// track (#631) lands, the text tables move to internal/i18n untouched and
// only classify.go (this file) stays behind referencing cli/session.
package usermsg

import (
	"context"
	"errors"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// Code is a stable, package-neutral classification of a send-path error.
// Values are NOT persisted or sent over the wire — they exist only to sever
// the text tables from the cli/session sentinel surface, so the int values
// may be reordered freely.
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
// match. key disambiguates the asleep case: a cron-namespace key yields
// CodeCronAsleep, every other key CodeSessionAsleep. A nil err is the
// caller's responsibility to short-circuit before calling classify.
func classify(err error, key string) Code {
	switch {
	case errors.Is(err, session.ErrMaxProcs):
		return CodeMaxProcs
	case errors.Is(err, session.ErrMaxExemptSessions):
		return CodeMaxExemptSessions
	case errors.Is(err, session.ErrNoCLIWrapper):
		return CodeNoCLIWrapper
	case errors.Is(err, session.ErrNoActiveProcess):
		if session.IsCronKey(key) {
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
	default:
		return CodeUnknown
	}
}

// isTimeout reports whether err is one of the timeout sentinels that
// UserMessage specialises with a concrete duration. Kept here so the only
// cli-import-needing predicate lives next to classify.
func isNoOutputTimeout(err error) bool { return errors.Is(err, cli.ErrNoOutputTimeout) }
func isTotalTimeout(err error) bool    { return errors.Is(err, cli.ErrTotalTimeout) }
