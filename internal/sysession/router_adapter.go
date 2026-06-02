package sysession

import (
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/api"
)

// RawSystemSessionRouter is the producer-side router shape, satisfied
// directly by the concrete *session.Router. It differs from the
// daemon-facing SystemSessionRouter on exactly one method:
// EventEntriesForKey here returns []cli.EventEntry (the router's native
// type) instead of the sysession-local []SystemEventEntry.
//
// This interface is the ONLY place in the package that mentions
// internal/cli. Config.Router accepts a RawSystemSessionRouter so
// wiring code (cmd/naozhi) can keep passing the bare *session.Router;
// NewManager immediately wraps it in routerAdapter so every daemon sees
// the cli-free SystemSessionRouter (R260528-ARCH-9 / #1370).
type RawSystemSessionRouter interface {
	// Embeds the shared streaming-read capability (R246-ARCH-11 /
	// R240-ARCH-15, #791 / #1032) so both router shapes in this package
	// reference one canonical VisitSessions signature.
	api.SessionVisitor
	SetUserLabelWithOrigin(key, label, origin string) bool
	ClearUserLabelOrigin(key string) bool
	RegisterSystemStub(key, workspace, lastPrompt string)
	EventEntriesForKey(key string) []cli.EventEntry
}

// routerAdapter bridges a RawSystemSessionRouter to the cli-free
// SystemSessionRouter consumed by daemons. Every method except
// EventEntriesForKey is a straight pass-through; EventEntriesForKey
// down-projects each cli.EventEntry onto the ≤2-field SystemEventEntry
// mirror so the daemon code path never references internal/cli.
type routerAdapter struct {
	raw RawSystemSessionRouter
}

// wrapRouter adapts a producer-side router into the daemon-facing
// interface. Returns nil when raw is nil so the Manager's nil-Router
// guard stays meaningful.
func wrapRouter(raw RawSystemSessionRouter) SystemSessionRouter {
	if raw == nil {
		return nil
	}
	return routerAdapter{raw: raw}
}

func (a routerAdapter) VisitSessions(fn func(session.SessionSnapshot) bool) {
	a.raw.VisitSessions(fn)
}

func (a routerAdapter) SetUserLabelWithOrigin(key, label, origin string) bool {
	return a.raw.SetUserLabelWithOrigin(key, label, origin)
}

func (a routerAdapter) ClearUserLabelOrigin(key string) bool {
	return a.raw.ClearUserLabelOrigin(key)
}

func (a routerAdapter) RegisterSystemStub(key, workspace, lastPrompt string) {
	a.raw.RegisterSystemStub(key, workspace, lastPrompt)
}

func (a routerAdapter) EventEntriesForKey(key string) []SystemEventEntry {
	raw := a.raw.EventEntriesForKey(key)
	if len(raw) == 0 {
		// Preserve the nil/empty distinction's only observable effect
		// (buildExcerptFromHistory treats both as "empty seed").
		return nil
	}
	out := make([]SystemEventEntry, len(raw))
	for i, e := range raw {
		out[i] = SystemEventEntry{Type: e.Type, Summary: e.Summary}
	}
	return out
}

// Compile-time guarantee that the concrete *session.Router satisfies the
// producer-side interface (and therefore can be passed to Config.Router).
var _ RawSystemSessionRouter = (*session.Router)(nil)
