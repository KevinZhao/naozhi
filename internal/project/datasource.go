// Package project — datasource.go: adapter that lets internal/session's
// KeyResolver read project data without importing this package. Satisfies
// projectapi.DataSource via a Manager pointer and translates *Project into
// projectapi.ProjectBinding; the shared contract types live in the neutral
// leaf internal/projectapi (#1373).
package project

import (
	"github.com/naozhi/naozhi/internal/projectapi"
)

// dataSource is the adapter; obtained via NewDataSource so nil handling is centralised.
type dataSource struct{ m *Manager }

// NewDataSource returns a projectapi.DataSource backed by m. It MUST return an
// untyped nil when m is nil so KeyResolver's `data != nil` guard disables
// project-aware routing instead of panicking on a typed-nil interface.
func NewDataSource(m *Manager) projectapi.DataSource {
	if m == nil {
		return nil
	}
	return &dataSource{m: m}
}

// ProjectBinding returns the project bound to the given chat, or a zero-value
// binding. Planner model/prompt precedence is delegated to Manager.
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
		Backend:       p.Config.Backend,
		AccessProfile: p.Config.AccessProfile,
	}
}

// ProjectByName looks up a project by name for the key-reverse path;
// ok=false when it does not exist (e.g. deleted before the Resolver call).
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
		Backend:       p.Config.Backend,
		AccessProfile: p.Config.AccessProfile,
	}, true
}
