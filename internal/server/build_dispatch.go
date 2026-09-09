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

// buildDispatcher wires the dispatcher from Server state that already exists
// at this point in buildServerWithHandlers (router, platforms, queue, guard,
// watchdog counters, appCtx).
func (s *Server) buildDispatcher() *dispatch.Dispatcher {
	// The nil guard must stay OUTSIDE the adapter: wrapping a nil scheduler in
	// a struct value yields a non-nil interface and breaks the "nil disables
	// /cron" contract (#1164).
	var cronCommands dispatch.CronCommands
	if s.scheduler != nil {
		cronCommands = cronDispatchAdapter{s: s.scheduler}
	}
	d, err := dispatch.NewDispatcher(dispatch.DispatcherConfig{
		Router:                s.router,
		Platforms:             s.platforms,
		Agents:                s.agents,
		AgentCommands:         s.agentCommands,
		Scheduler:             cronCommands,
		ProjectMgr:            s.projectMgr,
		Resolver:              s.resolver,
		Guard:                 s.sessionGuard,
		Queue:                 s.msgQueue,
		Dedup:                 s.dedup,
		AllowedRoot:           s.allowedRoot,
		ClaudeDir:             s.claudeDir,
		Capabilities:          serverCaps{s: s},
		NoOutputTimeout:       s.noOutputTimeout,
		TotalTimeout:          s.totalTimeout,
		WatchdogNoOutputKills: s.watchdog.noOutPtr(),
		WatchdogTotalKills:    s.watchdog.totalPtr(),
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
