package sysession

import (
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// SystemSessionRouter is the minimal slice of session.Router that the
// sysession package depends on.  Defined here (consumer-side) so:
//
//  1. Tests can inject fakes without pulling the whole router graph.
//  2. A future router refactor only needs to preserve these methods to
//     stay sysession-compatible — accidental surface-area growth becomes
//     a compile error in main.go's wiring instead of a silent regression.
//
// The concrete *session.Router automatically satisfies this interface;
// no helper required.
//
// This is the post-RFC-v2.1 shape:  no Reset (transient subprocess path
// doesn't share long-lived state), no Snapshot()-as-slice in the daemon
// hot path — VisitSessions is the streaming alternative, and dashboard
// one-shot reads go through ListSessions on *session.Router directly
// (this interface intentionally doesn't expose them so daemons stay on
// the streaming path).
type SystemSessionRouter interface {
	// VisitSessions streams every session through fn.  fn returning
	// false stops iteration early.  Used by AutoTitler to filter
	// candidates without materialising a slice.
	//
	// fn must NOT call back into Router methods that take r.mu (it
	// runs under RLock).  The idiomatic pattern is to copy fields the
	// daemon needs and resume work after the visit returns.
	VisitSessions(fn func(session.SessionSnapshot) bool)

	// SetUserLabelWithOrigin is the daemon-aware label writer.  It
	// MUST re-read LabelOrigin under r.mu before applying the write,
	// rejecting (return false) when origin=="auto" but the live origin
	// is "user".  See docs/rfc/system-session.md §11.1 for the race
	// invariant.
	SetUserLabelWithOrigin(key, label, origin string) bool

	// ClearUserLabelOrigin is the dashboard "restore auto naming"
	// path.  Implementation MUST clear both UserLabel AND LabelOrigin
	// so the legacy "empty origin = user-set" rule remains
	// unambiguous (RFC §7.3).  Returns false for unknown keys.
	ClearUserLabelOrigin(key string) bool

	// RegisterSystemStub is reserved for future daemons that need a
	// long-lived ManagedSession entry.  Phase 1 daemons (Runner-based)
	// don't use it.  Misuse with a non-sys: key panics — see
	// session.RegisterSystemStub.
	RegisterSystemStub(key, workspace, lastPrompt string)

	// EventEntriesForKey returns the event-log entries for the given
	// session key, or nil when unknown. Used by AutoTitler so the
	// rename prompt can review the entire user-turn history rather
	// than just the most recent prompt cached on SessionSnapshot.
	// Returns the live process's EventLog when the session is alive,
	// otherwise the persisted history slice.
	//
	// NEEDS-DESIGN (R242-ARCH-24): the []cli.EventEntry return type is
	// the only thing forcing this consumer-side interface to import
	// internal/cli. AutoTitler uses ≤4 fields per entry (Type, Text,
	// IsUser, Created) so a sysession-local SystemEventEntry mirror
	// + adapter at SystemSessionRouter.EventEntriesForKey would let
	// sysession drop the cli import entirely. Deferred until R243-
	// ARCH-12 (EventStore interface unification) lands so the mirror
	// shape can be derived from the central EventStore type rather
	// than coined ad-hoc here. Doing it twice (once now, once when
	// EventStore lands) would only churn AutoTitler tests.
	EventEntriesForKey(key string) []cli.EventEntry
}
