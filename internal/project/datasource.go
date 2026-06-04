// Package project — datasource.go
//
// Thin adapter that lets internal/session's KeyResolver read project data
// without importing the project package directly (reverse dependency chain —
// session is below project). The adapter satisfies projectapi.DataSource via a
// Manager pointer and translates *Project into projectapi.ProjectBinding.
//
// R260528-ARCH-12 (#1373): this file used to import internal/session to name
// PlannerDataSource / ProjectBinding, which made the project DOMAIN layer
// depend on the session ROUTING layer. The shared contract types now live in
// the neutral leaf internal/projectapi, so this adapter imports projectapi
// only — the reverse import is gone. session keeps aliases
// (session.PlannerDataSource = projectapi.DataSource), so dispatch's
// NewKeyResolver(agents, project.NewDataSource(mgr)) still type-checks.
package project

import (
	"github.com/naozhi/naozhi/internal/projectapi"
)

// dataSource is the adapter implementation. Kept unexported; callers
// obtain it via NewDataSource so nil-Manager handling is centralised.
type dataSource struct{ m *Manager }

// NewDataSource returns a projectapi.DataSource backed by the given Manager.
// Returns untyped nil interface when m is nil so a caller passing
// NewKeyResolver(agentDefaults, project.NewDataSource(nil)) correctly disables
// project-aware routing instead of producing a typed-nil interface (which
// would pass `data != nil` checks but panic on method call).
//
// MUST return untyped nil — return `&dataSource{m: nil}` would defeat the
// nil-guard in KeyResolver. Covered by
// TestNewDataSource_NilManagerReturnsNilInterface.
func NewDataSource(m *Manager) projectapi.DataSource {
	if m == nil {
		return nil
	}
	return &dataSource{m: m}
}

// ProjectBinding returns the project bound to the given chat, or a
// zero-value binding if no binding exists. Delegates the planner
// model/prompt precedence decisions to Manager so the "Effective*"
// rules stay authoritative in one place.
func (d *dataSource) ProjectBinding(platform, chatType, chatID string) projectapi.ProjectBinding {
	p := d.m.ProjectForChat(platform, chatType, chatID)
	if p == nil {
		return projectapi.ProjectBinding{}
	}
	return projectapi.ProjectBinding{
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
func (d *dataSource) ProjectByName(name string) (projectapi.ProjectBinding, bool) {
	p := d.m.Get(name)
	if p == nil {
		return projectapi.ProjectBinding{}, false
	}
	return projectapi.ProjectBinding{
		Bound:         true,
		Name:          p.Name,
		WorkspaceDir:  p.Path,
		PlannerModel:  d.m.EffectivePlannerModel(p),
		PlannerPrompt: d.m.EffectivePlannerPrompt(p),
	}, true
}
