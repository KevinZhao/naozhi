// Package projectapi holds the neutral, dependency-free contract types shared
// between the project (domain) layer and the session (routing) layer.
//
// R260528-ARCH-12 (#1373): ProjectBinding and the DataSource interface used to
// live in internal/session. That forced internal/project — the DOMAIN layer —
// to import internal/session — the ROUTING layer — purely to name the binding
// struct and the adapter interface its NewDataSource returns. The arrow
// pointed the wrong way (domain → routing).
//
// Hoisting both into this leaf package inverts nothing at runtime but rights
// the dependency direction: project and session both import projectapi, and
// neither imports the other for these types. session keeps thin aliases
// (session.ProjectBinding / session.PlannerDataSource) so its KeyResolver and
// every existing caller (dispatch) compile unchanged — the canonical
// definitions just live in a package that sits below both.
//
// projectapi MUST stay a leaf: it imports nothing from project / session /
// dispatch (only the standard library, if anything). Adding such an import
// would re-introduce the cycle this package exists to break.
package projectapi

// ProjectBinding is the minimal projection of a bound project that the session
// routing layer needs to derive keys and planner opts. It is populated by the
// project-package adapter (project.NewDataSource) via EffectivePlannerModel /
// EffectivePlannerPrompt, so the routing layer does NOT re-implement those
// precedence rules — they stay authoritative in project.Manager.
type ProjectBinding struct {
	Bound         bool
	Name          string
	WorkspaceDir  string
	PlannerModel  string // "" = inherit router / AgentDefaults
	PlannerPrompt string // "" = no --append-system-prompt
}

// DataSource abstracts the project-layer reads the session KeyResolver needs.
// The concrete implementation lives in the project package
// (project.NewDataSource); session never imports project directly. All methods
// return fully-snapshot'd values so callers can treat them as pure reads (no
// hidden mutex-state bleed).
//
// session.PlannerDataSource is a type alias for this interface, so the dense
// session-side call sites (KeyResolver.data, NewKeyResolver param) and the
// dispatch wiring that names session.PlannerDataSource keep working verbatim.
type DataSource interface {
	// ProjectBinding returns the project metadata for the given IM chat, or
	// zero-value (Bound == false) if the chat is not bound.
	ProjectBinding(platform, chatType, chatID string) ProjectBinding

	// ProjectByName returns the project metadata for the given planner key's
	// embedded project name. Used by the key-reverse path. ok == false when
	// the project cannot be found (e.g. operator deleted it between RPC
	// arrival and restart).
	ProjectByName(name string) (ProjectBinding, bool)
}
