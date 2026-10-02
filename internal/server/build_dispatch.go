// build_dispatch.go — construction of the IM-facing dispatcher (#2633).
//
// dispatch.NewDispatcher used to be called inside Start, because its StopCtx
// wanted the caller's ctx and Start was the first place that existed. E2
// #2552 gave the Server an appCtx at construction and made Start link the
// caller's ctx into it, so that reason is gone — but the call stayed, and with
// it a field (healthH.dispatcherMetrics) that was declared at construction and
// assigned in Start: the same setter-vs-Start window #431 closed everywhere
// else, spelled as a bare assignment `grep '\.Set[A-Z]'` could not see.
//
// buildServerWithHandlers now calls buildDispatcher before it builds
// HealthHandler, and Start only calls BuildHandler on the result.
package server

import (
	"fmt"

	"github.com/naozhi/naozhi/internal/dispatch"
)

// buildDispatcher wires the dispatcher from the Server state that already
// exists at this point in buildServerWithHandlers (router, platforms, watchdog
// counters, appCtx) and the construction-only dependencies in w.
func (s *Server) buildDispatcher(w *wiring) *dispatch.Dispatcher {
	// The nil guard must stay OUTSIDE the adapter: wrapping a nil scheduler in
	// a struct value yields a non-nil interface and breaks the "nil disables
	// /cron" contract (#1164).
	var cronCommands dispatch.CronCommands
	if w.scheduler != nil {
		cronCommands = cronDispatchAdapter{s: w.scheduler}
	}
	// Same for the router and resolver: a nil pointer boxed into the
	// interface field would defeat the dispatcher's nil checks.
	var router dispatch.SessionRouter
	if s.router != nil {
		router = dispatchRouter{s.router}
	}
	var resolver dispatch.KeyResolver
	if w.resolver != nil {
		resolver = w.resolver
	}
	// The engine comes from buildWSStack, which buildDashboard has run by
	// now; without it every IM send would nil-deref on its first message.
	caps := serverCaps{s: s, send: w.engine}
	if caps.send == nil {
		panic("server: buildDispatcher needs w.engine; buildDashboard must run first")
	}
	d, err := dispatch.NewDispatcher(dispatch.DispatcherConfig{
		Router:                router,
		Platforms:             s.platforms,
		Agents:                w.agents,
		AgentCommands:         w.agentCommands,
		Scheduler:             cronCommands,
		ProjectMgr:            s.projectMgr,
		Resolver:              resolver,
		Guard:                 w.sessionGuard,
		Queue:                 w.msgQueue,
		Dedup:                 w.dedup,
		AllowedRoot:           w.allowedRoot,
		ClaudeDir:             s.claudeDir,
		Capabilities:          caps,
		NoOutputTimeout:       s.noOutputTimeout,
		TotalTimeout:          s.totalTimeout,
		WatchdogNoOutputKills: w.watchdog.noOutPtr(),
		WatchdogTotalKills:    w.watchdog.totalPtr(),
		// Service ctx so the passthrough send goroutine observes SIGTERM
		// instead of waiting out its internal totalTimeout (#1320). appCtx is
		// cancelled by Start's linker when the caller's ctx is.
		StopCtx: s.appCtx,
	})
	if err != nil {
		// The only error NewDispatcher returns is ErrSendWireupMissing, and
		// serverCaps always carries Send — so this is a programming fault in
		// this package, not a configuration fault, and no config can reach
		// it. Fail at construction rather than on first message.
		panic(fmt.Sprintf("server: dispatch wireup: %v", err))
	}
	return d
}
