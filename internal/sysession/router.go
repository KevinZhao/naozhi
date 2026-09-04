package sysession

import (
	"github.com/naozhi/naozhi/internal/session/api"
)

// SystemEventEntry is the sysession-local mirror of the clievent.EventEntry
// fields AutoTitler reads, so the daemon-facing SystemSessionRouter does not
// import internal/cli (#1370). The conversion lives solely in
// router_adapter.go; widen this struct and the adapter together.
type SystemEventEntry struct {
	// Type mirrors clievent.EventEntry.Type; AutoTitler filters on "user".
	Type string
	// Summary mirrors clievent.EventEntry.Summary (brief per-turn text).
	Summary string
}

// SystemSessionRouter is the minimal slice of session.Router that sysession
// depends on, defined consumer-side so tests can inject fakes and router
// surface growth becomes a compile error in main.go's wiring. *session.Router
// satisfies it via routerAdapter. Daemons stay on the streaming VisitSessions
// path; ListSessions is intentionally not exposed.
type SystemSessionRouter interface {
	// VisitSessions (embedded from api.SessionVisitor) streams every session
	// through fn; fn returning false stops early. fn runs under RLock and MUST
	// NOT call back into Router methods that take r.mu — copy the fields the
	// daemon needs, then resume work after the visit returns (#791).
	api.SessionVisitor

	// SetUserLabelWithOrigin MUST re-read LabelOrigin under r.mu before
	// writing, returning false when origin=="auto" but the live origin is
	// "user" (race invariant: docs/rfc/system-session.md §11.1).
	SetUserLabelWithOrigin(key, label, origin string) bool

	// ClearUserLabelOrigin MUST clear both UserLabel AND LabelOrigin so the
	// "empty origin = user-set" rule stays unambiguous (RFC §7.3). Returns
	// false for unknown keys.
	ClearUserLabelOrigin(key string) bool

	// RegisterSystemStub is reserved for daemons needing a long-lived
	// ManagedSession entry; a non-sys: key panics (session.RegisterSystemStub).
	RegisterSystemStub(key, workspace, lastPrompt string)

	// EventEntriesForKey returns the event-log entries for key (live EventLog
	// when alive, else persisted history), or nil when unknown. Returns the
	// cli-free SystemEventEntry mirror; routerAdapter bridges from
	// []clievent.EventEntry (#1370).
	EventEntriesForKey(key string) []SystemEventEntry
}
