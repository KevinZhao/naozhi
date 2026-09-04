// Package system hosts the dashboard /api/system/* endpoints: the sysession
// daemon status list, the label-origin reset, and the self-update state /
// apply pair. All sit behind the /api/* auth middleware.
//
//	GET  /api/system/daemons             read-only daemon status list
//	POST /api/system/labels/clear-origin reset a session's LabelOrigin
//	GET  /api/system/update              version state + what the operator can do
//	POST /api/system/update/apply        carry it out (install and/or restart)
package system

import (
	"context"

	"github.com/naozhi/naozhi/internal/ratelimit"
	"github.com/naozhi/naozhi/internal/selfupdate"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sysession"
)

// Router is the consumer-side subset of *session.Router these handlers use,
// so the sub-package never imports internal/server.
type Router interface {
	ListSessions() []session.SessionSnapshot
	ClearUserLabelOrigin(key string) bool
}

// DaemonInspector is the consumer-side subset of *sysession.Manager. A nil
// value means sysession is disabled; the daemons endpoint then serves [].
type DaemonInspector interface {
	Inspector() []sysession.DaemonStatus
}

// Deps carries what the handlers read; the server wires it once at build.
type Deps struct {
	// Daemons is nil when sysession is disabled (must be a nil interface,
	// not a nil *Manager, or the disabled path never triggers).
	Daemons DaemonInspector
	Router  Router
	// UpdateStatus / UpdateChecker are nil when the checker is disabled; GET
	// then reports only BuildVersion.
	UpdateStatus  *selfupdate.Status
	UpdateChecker *selfupdate.Checker
	BuildVersion  string
	// InstallEnabled gates POST .../apply (update.dashboard_install).
	InstallEnabled bool
}

// Handlers serves the /api/system/* endpoint family.
type Handlers struct {
	daemons        DaemonInspector
	router         Router
	updateStatus   *selfupdate.Status
	updateChecker  *selfupdate.Checker
	buildVersion   string
	installEnabled bool
	// applyLimiter is global (single bucket), see newUpdateApplyLimiter.
	applyLimiter *ratelimit.Limiter
	// applyFn is a test seam; nil ⇒ updateChecker.InstallLatest.
	applyFn func(ctx context.Context, restart bool) error
}

// New returns Handlers wired from d.
func New(d Deps) *Handlers {
	return &Handlers{
		daemons:        d.Daemons,
		router:         d.Router,
		updateStatus:   d.UpdateStatus,
		updateChecker:  d.UpdateChecker,
		buildVersion:   d.BuildVersion,
		installEnabled: d.InstallEnabled,
		applyLimiter:   newUpdateApplyLimiter(),
	}
}
