package session

import "github.com/naozhi/naozhi/internal/session/sessionview"

// The value types consumers exchange with the router live in sessionview, a
// leaf package, so a consumer can depend on them without importing session.
// These aliases keep the session.* spelling; they are the same types.
type (
	AgentOpts         = sessionview.AgentOpts
	SessionStatus     = sessionview.SessionStatus
	SessionSnapshot   = sessionview.SessionSnapshot
	OverlayFieldDrift = sessionview.OverlayFieldDrift
	InterruptOutcome  = sessionview.InterruptOutcome
	BackendManifest   = sessionview.BackendManifest
)

const (
	SessionExisting = sessionview.SessionExisting
	SessionResumed  = sessionview.SessionResumed
	SessionNew      = sessionview.SessionNew

	InterruptSent        = sessionview.InterruptSent
	InterruptNoSession   = sessionview.InterruptNoSession
	InterruptNoTurn      = sessionview.InterruptNoTurn
	InterruptUnsupported = sessionview.InterruptUnsupported
	InterruptError       = sessionview.InterruptError
)
