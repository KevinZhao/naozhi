package server

import (
	dashsession "github.com/naozhi/naozhi/internal/dashboard/session"
	"github.com/naozhi/naozhi/internal/session"
)

// Compile-time guard so method-set drift lands here, next to the adapter.
var _ dashsession.RouterView = sessionRouterView{}

// sessionRouterView adapts *session.Router to dashsession.RouterView. The
// two CLI facts the stats payload folds in live on the router's backend
// facet; everything else is promoted unchanged.
type sessionRouterView struct{ *session.Router }

func (v sessionRouterView) CLIName() string { return v.Backends().CLIName() }

func (v sessionRouterView) CLIVersion() string { return v.Backends().CLIVersion() }
