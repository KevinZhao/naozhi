package sysession

import (
	"strings"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/api"
)

// RawSystemSessionRouter is the producer-side router shape satisfied directly
// by *session.Router; it differs from SystemSessionRouter only in
// EventEntriesForKey returning the native []clievent.EventEntry. It is the
// ONLY place in the package that mentions internal/cli: Config.Router accepts
// it and NewManager wraps it in routerAdapter (#1370).
type RawSystemSessionRouter interface {
	// Shared streaming-read capability: one canonical VisitSessions signature (#791).
	api.SessionVisitor
	SetUserLabelWithOrigin(key, label, origin string) bool
	ClearUserLabelOrigin(key string) bool
	RegisterSystemStub(key, workspace, lastPrompt string)
	EventEntriesForKey(key string) []clievent.EventEntry
}

// routerAdapter bridges a RawSystemSessionRouter to the cli-free
// SystemSessionRouter. Every method except EventEntriesForKey is a straight
// pass-through; EventEntriesForKey projects onto SystemEventEntry and drops
// non-user / blank-summary entries at this single conversion point instead
// of copying all ~500 ring entries for buildExcerptFromHistory to re-filter.
// AutoTitler is the sole consumer and only reads such turns (#1578).
type routerAdapter struct {
	raw RawSystemSessionRouter
}

// wrapRouter adapts a producer-side router; returns nil for nil raw so the
// Manager's nil-Router guard stays meaningful.
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
		// buildExcerptFromHistory treats nil and empty alike ("empty seed").
		return nil
	}
	// Project + filter in one pass (#1578); returning nil when nothing
	// survives keeps the empty-seed contract.
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

// Compile-time guarantee that *session.Router satisfies the producer-side interface.
var _ RawSystemSessionRouter = (*session.Router)(nil)
