package server

import (
	"time"

	dashsession "github.com/naozhi/naozhi/internal/dashboard/session"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/runhistory"
)

// Compile-time guard so method-set drift lands here, next to the adapter.
var _ dashsession.RouterView = sessionRouterView{}

// sessionRouterView adapts *session.Router to dashsession.RouterView. The
// two CLI facts the stats payload folds in live on the router's backend
// facet, and the two run-history methods live on its RunLedger facet
// (Router.Runs()); everything else is promoted unchanged.
type sessionRouterView struct{ *session.Router }

func (v sessionRouterView) CLIName() string { return v.Backends().CLIName() }

func (v sessionRouterView) CLIVersion() string { return v.Backends().CLIVersion() }

func (v sessionRouterView) SessionRuns(key string, limit int, before time.Time) []runhistory.SessionRun {
	return v.Runs().List(key, limit, before)
}

func (v sessionRouterView) SessionRunStats(key string) runhistory.SessionRunStats {
	return v.Runs().Stats(key)
}
