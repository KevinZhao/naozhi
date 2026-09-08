// consumer.go — the consumer-side dependency interfaces this package needs
// (#2561).
//
// Deps named concrete types: *session.Router (77 exported methods),
// *session.KeyResolver, *project.Manager, *node.CacheManager. Each interface
// below is the MEASURED call surface — grep h.<field>.<Method> across the
// package — and the result is the point: this package needed 3 Router methods
// out of 77 and one KeyResolver method.
//
// Declared here rather than in internal/dashboard/contracts because these are
// per-consumer shapes: dashsession uses 18 Router methods, this package uses 3,
// and a shared interface would be the union, i.e. a wide type again. contracts
// is for shapes several sub-packages genuinely share (IPLimiter).
// docs/rfc/consumer-interfaces.md §3.1.
//
// CAUTION for the next conversion of this kind: these fields are nil-guarded
// downstream (projectMgr ×9, router ×2, resolver ×1) because each is optional.
// Handing an interface field a nil CONCRETE pointer makes `!= nil` read true and
// the guard useless — see TestProjectHandlers_NilDepsStayNilInterfaces and the
// same hazard in #377 / #2551 / #2561's dashsession conversion. The wiring site
// unwraps typed nils.
package project

import (
	"context"

	projectpkg "github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// ProjectStore is the *project.Manager surface: the project list, one project,
// the two writes reachable from the UI, and the two planner-config resolvers.
type ProjectStore interface {
	All() []*projectpkg.Project
	Get(name string) *projectpkg.Project
	SetFavorite(name string, favorite bool) error
	UpdateConfig(name string, cfg projectpkg.ProjectConfig) error
	EffectivePlannerModel(p *projectpkg.Project) string
	EffectivePlannerPrompt(p *projectpkg.Project) string
}

// RouterView is the 3 *session.Router methods this package calls, out of 77:
// the planner-restart path (look up, recreate) plus the version bump that makes
// the dashboard re-render.
type RouterView interface {
	SessionFor(key string) *session.ManagedSession
	ResetAndRecreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, error)
	BumpVersion()
}

// PlannerKeyResolver is the one *session.KeyResolver method this package needs:
// project name → planner session key + opts.
type PlannerKeyResolver interface {
	ResolveForPlannerKey(projectName string) (key string, opts session.AgentOpts, ok bool)
}

// NodeCacheReader is the *node.CacheManager surface: the cached remote-node
// project list folded into /api/projects.
type NodeCacheReader interface {
	Projects() map[string][]map[string]any
}

// DepsForTest exposes the three nil-guarded interface deps so the wiring side
// can assert none of them is an interface wrapping a nil pointer (#2561; see
// TestDashboardDeps_NilStayNilInterfaces). Keyed by name so the failure message
// can say which one.
func (h *Handlers) DepsForTest() map[string]any {
	return map[string]any{
		"dashproject.ProjectMgr": h.projectMgr,
		"dashproject.Router":     h.router,
		"dashproject.Resolver":   h.resolver,
	}
}
