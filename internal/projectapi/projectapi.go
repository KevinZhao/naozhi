// Package projectapi holds the neutral, dependency-free contract types shared
// between the project (domain) layer and the session (routing) layer, so
// neither imports the other for them (#1373). It MUST stay a leaf: importing
// project / session / dispatch would re-introduce the cycle it exists to break.
package projectapi

// ProjectBinding is the minimal projection of a bound project the routing
// layer needs to derive keys and planner opts. project.NewDataSource populates
// it via EffectivePlannerModel/Prompt, so precedence stays in project.Manager.
type ProjectBinding struct {
	Bound         bool
	Name          string
	WorkspaceDir  string
	PlannerModel  string // "" = inherit router / AgentDefaults
	PlannerPrompt string // "" = no --append-system-prompt
	// Backend is the project's default CLI backend ("" = router default).
	Backend string
	// AccessProfile is the project's default access-profile ID ("" = global
	// default); only the name — env values live in the trusted config.
	AccessProfile string
}

// DataSource abstracts the project-layer reads the session KeyResolver needs;
// the concrete implementation is project.NewDataSource. All methods return
// snapshot values so callers can treat them as pure reads.
type DataSource interface {
	// ProjectBinding returns the project metadata for the given IM chat, or
	// zero-value (Bound == false) if the chat is not bound.
	ProjectBinding(platform, chatType, chatID string) ProjectBinding

	// ProjectByName resolves a planner key's embedded project name; ok == false
	// when the project cannot be found.
	ProjectByName(name string) (ProjectBinding, bool)
}
