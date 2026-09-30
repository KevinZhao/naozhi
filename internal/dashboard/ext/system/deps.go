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
	"time"

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
	UpdateStatus  UpdateStatus
	UpdateChecker UpdateChecker
	BuildVersion  string
	// InstallEnabled gates POST .../apply.
	InstallEnabled bool
}

// Handlers serves the /api/system/* endpoint family.
type Handlers struct {
	daemons        DaemonInspector
	router         Router
	updateStatus   UpdateStatus
	updateChecker  UpdateChecker
	buildVersion   string
	installEnabled bool
	// applyLimiter is global (single bucket), see newUpdateApplyLimiter.
	applyLimiter *ratelimit.Limiter
	// applyFn is a test seam; nil ⇒ updateChecker.InstallLatest.
	applyFn func(ctx context.Context, restart bool) error
}

// New returns Handlers wired from d.
func New(d Deps) *Handlers {
	status := d.UpdateStatus
	if status == nil {
		// *selfupdate.Status is nil-receiver safe — a zero snapshot, no-op
		// marks — so a nil one is the checker-disabled status the handlers
		// call unguarded, exactly what a zero Deps carried before this field
		// was an interface.
		status = (*selfupdate.Status)(nil)
	}
	return &Handlers{
		daemons:        d.Daemons,
		router:         d.Router,
		updateStatus:   status,
		updateChecker:  d.UpdateChecker,
		buildVersion:   d.BuildVersion,
		installEnabled: d.InstallEnabled,
		applyLimiter:   newUpdateApplyLimiter(),
	}
}

// UpdateStatus is the *selfupdate.Status surface: the checker's last result
// and the apply path's failure mark.
type UpdateStatus interface {
	Snapshot() selfupdate.StatusSnapshot
	LastCheck() (at time.Time, latest string)
	MarkFailed(err error)
}

// UpdateChecker is the *selfupdate.Checker surface: an on-demand check and the
// install it applies. A nil interface means the checker is disabled; the wiring
// site must not box a nil *selfupdate.Checker into it.
type UpdateChecker interface {
	CheckNow(ctx context.Context) error
	InstallLatest(ctx context.Context, restart bool) error
}
