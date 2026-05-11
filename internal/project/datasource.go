// Package project — datasource.go
//
// Thin adapter that lets internal/session's KeyResolver read project
// data without importing the project package directly (reverse
// dependency chain — session is below project). The adapter satisfies
// session.PlannerDataSource via a Manager pointer and translates
// *Project into session.ProjectBinding.
package project

import (
	"github.com/naozhi/naozhi/internal/session"
)

// dataSource is the adapter implementation. Kept unexported; callers
// obtain it via NewDataSource so nil-Manager handling is centralised.
type dataSource struct{ m *Manager }

// NewDataSource returns a session.PlannerDataSource backed by the given
// Manager. Returns untyped nil interface when m is nil so a caller
// passing NewKeyResolver(agentDefaults, project.NewDataSource(nil))
// correctly disables project-aware routing instead of producing a
// typed-nil interface (which would pass `data != nil` checks but panic
// on method call).
//
// MUST return untyped nil — return `&dataSource{m: nil}` would defeat
// the nil-guard in KeyResolver. Covered by
// TestNewDataSource_NilManagerReturnsNilInterface.
func NewDataSource(m *Manager) session.PlannerDataSource {
	if m == nil {
		return nil
	}
	return &dataSource{m: m}
}

// ProjectBinding returns the project bound to the given chat, or a
// zero-value binding if no binding exists. Delegates the planner
// model/prompt precedence decisions to Manager so the "Effective*"
// rules stay authoritative in one place.
func (d *dataSource) ProjectBinding(platform, chatType, chatID string) session.ProjectBinding {
	p := d.m.ProjectForChat(platform, chatType, chatID)
	if p == nil {
		return session.ProjectBinding{}
	}
	return session.ProjectBinding{
		Bound:         true,
		Name:          p.Name,
		WorkspaceDir:  p.Path,
		PlannerModel:  d.m.EffectivePlannerModel(p),
		PlannerPrompt: d.m.EffectivePlannerPrompt(p),
	}
}

// ProjectByName looks up a project by name for the key-reverse path.
// Returns ok=false when the project does not exist (e.g. deleted between
// RPC arrival and Resolver call).
func (d *dataSource) ProjectByName(name string) (session.ProjectBinding, bool) {
	p := d.m.Get(name)
	if p == nil {
		return session.ProjectBinding{}, false
	}
	return session.ProjectBinding{
		Bound:         true,
		Name:          p.Name,
		WorkspaceDir:  p.Path,
		PlannerModel:  d.m.EffectivePlannerModel(p),
		PlannerPrompt: d.m.EffectivePlannerPrompt(p),
	}, true
}
