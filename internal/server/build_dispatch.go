// build_dispatch.go — construction of the IM-facing dispatcher (#2633).
//
// buildServerWithHandlers calls buildDispatcher before it builds
// HealthHandler, so the dispatcher's metrics closure is a constructor
// argument; Start only calls BuildHandler on the result. The dispatcher's
// turns run on the turn.Orchestrator buildWSStack built for the dashboard
// engine too; it lives in wiring, not on Server.
package server

import (
	"fmt"
	"time"

	"golang.org/x/time/rate"

	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/imbudget"
	"github.com/naozhi/naozhi/internal/ratelimit"
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
		router = s.router
	}
	var resolver dispatch.KeyResolver
	if w.resolver != nil {
		resolver = w.resolver
	}
	// The Orchestrator comes from buildWSStack, which buildDashboard has run
	// by now; a nil one would fail every IM message at Submit.
	if w.turns == nil {
		panic("server: buildDispatcher needs w.turns; buildDashboard must run first")
	}
	d, err := dispatch.NewDispatcher(dispatch.DispatcherConfig{
		Router:                router,
		Platforms:             s.platforms,
		Agents:                w.agents,
		AgentCommands:         w.agentCommands,
		Scheduler:             cronCommands,
		ProjectMgr:            s.projectMgr,
		Resolver:              resolver,
		Turns:                 w.turns,
		Dedup:                 w.dedup,
		AllowedRoot:           w.allowedRoot,
		Access:                w.imAccess,
		Budget:                s.buildBudgetGate(w.imLimits.Budget),
		UserRate:              buildUserLimiter(w.imLimits),
		ClaudeDir:             s.claudeDir,
		Capabilities:          serverCaps{s: s},
		NoOutputTimeout:       s.noOutputTimeout,
		TotalTimeout:          s.totalTimeout,
		WatchdogNoOutputKills: w.watchdog.noOutPtr(),
		WatchdogTotalKills:    w.watchdog.totalPtr(),
		// Service ctx so a detached IM turn observes SIGTERM instead of
		// waiting out its internal totalTimeout (#1320). appCtx is cancelled
		// by Start's linker when the caller's ctx is.
		StopCtx: s.appCtx,
	})
	if err != nil {
		// The only error NewDispatcher returns is ErrTurnsWireupMissing, and
		// Turns is always set above — so this is a programming fault in this
		// package, not a configuration fault. Fail at construction rather
		// than on first message.
		panic(fmt.Sprintf("server: dispatch wireup: %v", err))
	}
	return d
}

// buildBudgetGate builds the per-chat spend gate over the router's cost
// ledger; nil when the policy is off or there is no ledger to read (config
// validation already refuses a budget with cost.enabled false).
func (s *Server) buildBudgetGate(p imbudget.Policy) *imbudget.Gate {
	if !p.Enabled() || s.router == nil {
		return nil
	}
	ledger := s.router.Runs().CostLedger()
	if !ledger.Enabled() {
		return nil
	}
	return imbudget.New(p, ledgerSpendUSD(ledger))
}

// ledgerSpendUSD sums a chat's USD entries in [from, to); credits and token
// units are not converted (docs/rfc/im-usage-limits.md §2).
func ledgerSpendUSD(ledger *costledger.Store) imbudget.SpendFunc {
	return func(chatKey string, from, to time.Time) (float64, error) {
		sum, err := ledger.Summarize(costledger.Query{From: from, To: to, ChatKey: chatKey, GroupBy: costledger.GroupByUnit})
		if err != nil {
			return 0, err
		}
		for _, b := range sum.Buckets {
			if b.Unit == costledger.UnitUSD {
				return b.Amount, nil
			}
		}
		return 0, nil
	}
}

// buildUserLimiter builds the per-sender message limiter; nil when off.
func buildUserLimiter(o IMLimitsOptions) *ratelimit.Limiter {
	if o.UserRatePerMinute <= 0 {
		return nil
	}
	burst := o.UserRateBurst
	if burst <= 0 {
		burst = o.UserRatePerMinute
	}
	return ratelimit.New(ratelimit.Config{
		Rate:  rate.Every(time.Minute / time.Duration(o.UserRatePerMinute)),
		Burst: burst,
	})
}
