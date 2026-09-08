package server

import (
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"

	"github.com/naozhi/naozhi/internal/cron"
	dashcron "github.com/naozhi/naozhi/internal/dashboard/cron"
	dashproject "github.com/naozhi/naozhi/internal/dashboard/project"
	dashsession "github.com/naozhi/naozhi/internal/dashboard/session"
)

// Compile-time assertion that *session.Router satisfies HubRouter, the
// *Hub-only consumer subset declared in consumer.go. *Hub embeds the
// concrete *session.Router behind this interface today; the assertion
// catches signature drift at build time so a Router rename breaks the
// build instead of silently breaking structural typing. R222-CR-10.
var _ HubRouter = (*session.Router)(nil)

// Compile-time assertion that *Hub satisfies HubBroadcaster, the
// Broadcaster facet declared in consumer.go (R237-ARCH-10). This pins the
// broadcast/fan-out surface as a named seam so the eventual ConnPool /
// Broadcaster / SendPath / AgentLinker struct split can carve these
// methods onto a dedicated type without silently dropping or renaming
// one — a signature drift breaks the build here instead of leaving the
// facet contract stale.
var _ HubBroadcaster = (*Hub)(nil)

// Compile-time assertions for the sendEngine seam (#2551, RFC
// send-engine-extraction §2.6): *session.Router must satisfy the engine's
// 12-method router subset, and *Hub must satisfy the 4-method broadcast exit
// the engine reaches for. Both are the drift guards that keep the engine from
// silently re-widening back onto HubRouter / the whole Hub.
var _ sendEngineRouter = (*session.Router)(nil)
var _ sendNotifier = (*Hub)(nil)

// Compile-time assertions for the dashsession consumer interfaces (#2561).
// Declared HERE, at the wiring site, rather than in internal/dashboard/session:
// that package must not import internal/session's concrete Router to assert it,
// or the narrowing would be undone by the assertion itself. server already
// imports both sides.
var (
	_ dashsession.RouterView      = (*session.Router)(nil)
	_ dashsession.ProjectSource   = (*project.Manager)(nil)
	_ dashsession.NodeCacheReader = (*node.CacheManager)(nil)
	_ dashsession.RetiredReader   = (*discovery.RetiredStore)(nil)
)

// Same for dashproject and dashcron (#2561 E6-b). The measured narrowing is in
// each package's consumer.go: 3 of *session.Router's 77 methods for
// dashproject, 23 of *cron.Scheduler's 48 for dashcron.
var (
	_ dashproject.ProjectStore       = (*project.Manager)(nil)
	_ dashproject.RouterView         = (*session.Router)(nil)
	_ dashproject.PlannerKeyResolver = (*session.KeyResolver)(nil)
	_ dashproject.NodeCacheReader    = (*node.CacheManager)(nil)
	_ dashcron.SchedulerView         = (*cron.Scheduler)(nil)
)
