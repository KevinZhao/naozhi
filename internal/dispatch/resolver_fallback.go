package dispatch

// resolver_fallback.go is the one place dispatch names internal/session: a
// Dispatcher built without a Resolver (headless and test constructions)
// adopts the router's shared KeyResolver or builds a project-less one, and
// only session can construct it. Everything else in the package goes through
// the consumer interfaces in consumer.go.

import (
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// routerResolverSource is the optional router capability the fallback chain
// reads: the router-attached singleton (#604). The production router adapter
// has it by embedding *session.Router.
type routerResolverSource interface {
	Resolver() *session.KeyResolver
}

// resolveOrFabricateKeyResolver returns the KeyResolver Dispatcher holds.
// Precedence (single track — do not copy this chain elsewhere, #543):
//
//  1. cfg.Resolver
//  2. cfg.Router.Resolver() (Router-attached singleton, #604)
//  3. a fresh resolver from cfg.Agents + project data source (nil-safe)
//
// Always non-nil, so callers dereference d.resolver without a guard.
func resolveOrFabricateKeyResolver(cfg DispatcherConfig) KeyResolver {
	if cfg.Resolver != nil {
		return cfg.Resolver
	}
	if rs, ok := cfg.Router.(routerResolverSource); ok {
		if r := rs.Resolver(); r != nil {
			return r
		}
	}
	var data session.PlannerDataSource
	if cfg.ProjectMgr != nil {
		data = project.NewDataSource(cfg.ProjectMgr)
	}
	return session.NewKeyResolver(cfg.Agents, data)
}
