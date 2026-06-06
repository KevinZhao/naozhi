package sysession

import (
	"strings"

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
//
// R20260602-PERF-1 (#1578): the projection also drops non-user and
// blank-summary entries here, at the single conversion point, instead of
// copying all ~500 ring entries and re-filtering them in
// buildExcerptFromHistory. AutoTitler is the sole consumer and only ever
// reads type=="user" turns with non-empty summaries, so this is
// behaviour-equivalent while shrinking both the copy and the downstream
// walk to just the entries that survive.
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
	// Project + filter in one pass: keep only the type=="user" turns with
	// a non-blank summary that buildExcerptFromHistory would have kept
	// anyway (R20260602-PERF-1 / #1578). Worst case (all-user) this still
	// allocates len(raw); typical ring traffic (~70% non-user) shrinks the
	// slice ~3×. Returning nil when nothing survives keeps the empty-seed
	// contract identical to the pre-filter behaviour.
	out := make([]SystemEventEntry, 0, len(raw))
	for _, e := range raw {
		if e.Type != "user" || strings.TrimSpace(e.Summary) == "" {
			continue
		}
		out = append(out, SystemEventEntry{Type: e.Type, Summary: e.Summary})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Compile-time guarantee that the concrete *session.Router satisfies the
// producer-side interface (and therefore can be passed to Config.Router).
var _ RawSystemSessionRouter = (*session.Router)(nil)
