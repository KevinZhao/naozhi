// handler_set.go — the HTTP handlers that exist only to be mounted (#2553).
//
// Seventeen handler pointers used to live on Server for the whole process
// lifetime while being touched in exactly two places: the constructor and the
// route registration a few lines later. That is the "god struct + N views"
// shape server-split-phase4-design.md §六 rejected, and its prescribed terminal
// state is explicit: a registration-only handler becomes a routes.go local, not
// a permanent field.
//
// handlerSet is that local. buildServer creates one, registers the routes from
// it, and lets it fall out of scope. Server keeps a pointer only to the three
// handlers something OTHER than registration needs:
//
//	auth        — debug_expvar / debug_pprof / ccassets wrappers, RotateDashboardSessions
//	sessionH    — retired-store flusher loop, WarmHistory + Flush on shutdown
//	discoveryH  — Wait() drains takeover goroutines during shutdown
//
// Those three are lifecycle participants, not views, so they stay. Everything
// else is unreachable after registration and has no business outliving it; a
// test that drives one of those handlers directly receives the set from
// buildServerWithHandlers.
//
// Field names deliberately match the old Server field names: the routes
// snapshot resolves a handler's type by the outermost selector's FIELD name
// (routes_snapshot_test.go serverFieldType), so `hs.cronH.handleList` and
// `s.cronH.handleList` produce the same golden entry. Renaming a field here
// requires updating that map in the same commit.
package server

import (
	"time"

	dashcost "github.com/naozhi/naozhi/internal/dashboard/cost"
	dashcron "github.com/naozhi/naozhi/internal/dashboard/cron"
	"github.com/naozhi/naozhi/internal/dashboard/discovery"
	"github.com/naozhi/naozhi/internal/dashboard/ext/accessprofile"
	"github.com/naozhi/naozhi/internal/dashboard/ext/agentevents"
	extccassets "github.com/naozhi/naozhi/internal/dashboard/ext/ccassets"
	"github.com/naozhi/naozhi/internal/dashboard/ext/cli"
	"github.com/naozhi/naozhi/internal/dashboard/ext/memory"
	"github.com/naozhi/naozhi/internal/dashboard/ext/planner"
	"github.com/naozhi/naozhi/internal/dashboard/ext/scratch"
	"github.com/naozhi/naozhi/internal/dashboard/ext/system"
	"github.com/naozhi/naozhi/internal/dashboard/ext/transcribe"
	"github.com/naozhi/naozhi/internal/dashboard/ext/uisettings"
	dashproject "github.com/naozhi/naozhi/internal/dashboard/project"
	dashsession "github.com/naozhi/naozhi/internal/dashboard/session"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/routerrelay"
	"github.com/naozhi/naozhi/internal/runtelemetry"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sysession"
	"github.com/naozhi/naozhi/internal/turn"
)

// handlerSet carries the dashboard handlers from construction to route
// registration. It is a buildServer local; nothing may store it.
type handlerSet struct {
	// wiring is what the build steps read and nothing keeps (see wiring).
	wiring *wiring

	cronH           *dashcron.Handlers
	transcribeH     *transcribe.Handler
	projectH        *dashproject.Handlers
	costH           *dashcost.Handlers
	sendH           *SendHandler
	cliH            *cli.Handler
	scratchH        *scratch.Handler
	memoryH         *memory.Handler
	ccAssetsH       *extccassets.Handler
	agentEventsH    *agentevents.Handler
	uiSettingsH     *uisettings.Handler
	systemH         *system.Handlers
	plannerH        *planner.Handlers
	accessProfilesH *accessprofile.Handler
	// healthH serves the server-owned probes (/health, /livez, /readyz).
	healthH *HealthHandler

	// The three lifecycle handlers are held here too, because registration
	// needs them like any other. Server holds the same pointers for the
	// non-registration work listed in this file's header — one instance, two
	// references, same as the Hub/engine split in #2551.
	sessionH   *dashsession.Handlers
	discoveryH *discovery.Handlers
}

// checkLimiters fails the boot rather than serving an endpoint whose rate
// limiter was never wired. The cron and cost handlers nil-guard their limiters
// so partially-constructed test fixtures work, which means a refactor that
// forgets one would silently run unlimited — the fail-fast has to live at the
// construction site, not in the handler.
func (hs *handlerSet) checkLimiters(schedulerWired bool) {
	if hs.costH != nil && !hs.costH.HasLimiter() {
		panic("server: cost limiter must be non-nil")
	}
	if !schedulerWired || hs.cronH == nil {
		return
	}
	if !hs.cronH.HasRunsLimiter() {
		panic("server: runsLimiter must be non-nil when scheduler is wired")
	}
	if !hs.cronH.HasListLimiter() {
		panic("server: listLimiter must be non-nil when scheduler is wired")
	}
	if !hs.cronH.HasWriteLimiter() {
		panic("server: writeLimiter must be non-nil when scheduler is wired")
	}
	if !hs.cronH.HasTranscriptLimiter() {
		panic("server: transcriptLimiter must be non-nil when scheduler is wired")
	}
}

// wiring holds the dependencies buildServerWithHandlers creates for the build
// steps alone — the dispatcher, the Hub, the dashboard handlers and route
// registration read them, and once those hold their own references nothing on
// Server needs them. A Server field only construction reads is a field every
// later reader has to rule out (#2897 S4).
type wiring struct {
	dedup         *platform.Dedup
	queue         turn.QueueOptions // buildWSStack builds turns' queue from it
	startedAt     time.Time
	agents        map[string]session.AgentOpts
	agentCommands map[string]string
	allowedRoot   string
	debugMode     bool // gates /api/debug/pprof and /api/debug/vars
	resolver      *session.KeyResolver
	sysessionMgr  *sysession.Manager
	orient        *orientConfig // nil = image auto-orientation off
	scheduler     cronScheduler // nil when cron is not configured (see buildServerWithHandlers)
	// routerEvents is opts.Relays.Router; buildDashboard binds the hub to it.
	routerEvents *routerrelay.Relay
	// runTelemetry is opts.Relays.RunTelemetry; buildDashboard binds the hub to it.
	runTelemetry *runtelemetry.Relay
	// watchdog holds the no-output / total watchdog-kill counters; the
	// dispatcher, the session handlers and /health each get pointers into it.
	watchdog watchdogCounters
	// engine, bcast and turns are set by buildWSStack; the dispatcher,
	// SendHandler and the broadcast producers take them from here, not from
	// the Hub.
	engine *sendEngine
	bcast  *wsBroadcaster
	turns  *turn.Orchestrator
}
