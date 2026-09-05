package cron

// deps.go declares SchedulerDeps, the dependency half of the cfg/deps split
// (docs/rfc/cron-sysession-merge.md §3.5.1, #746): interface / func / map
// fields live here; scalar config stays on SchedulerConfig. context.Context
// counts as a lifecycle scalar, so ParentCtx (and the AllowNilRouter bool)
// stay on SchedulerConfig. Pinned by deps_boundary_test.go.

import (
	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// SchedulerDeps carries the injected components the cron Scheduler talks to.
// All fields are optional (nil/empty = feature off) except Router, which is
// required in production — see SchedulerConfig and AllowNilRouter.
type SchedulerDeps struct {
	// Router accepts the SessionRouter interface so tests can pass a minimal
	// fake; production passes a *session.Router.
	Router SessionRouter
	// NotifySender resolves a platform name to its PlatformReplier for cron
	// completion notices; the wireup layer builds the adapter so internal/cron
	// never imports internal/platform (#725). nil = no notify delivery.
	NotifySender  NotifySender
	Agents        map[string]AgentOpts
	AgentCommands map[string]string
	// Telemetry receives RunStartedEvent / RunEndedEvent for every cron run.
	// nil = no broadcast. cmd/naozhi builds the Scheduler before the Hub
	// exists and injects late via SetTelemetry; both paths coexist.
	Telemetry runtelemetry.Broadcaster
	// Sandbox executes placement=sandbox jobs on AgentCore microVMs
	// (agentcore-cloud-sandbox RFC §4.2); built by the wireup layer so cron
	// never imports the AWS SDK. nil = such jobs terminate with
	// ErrClassCronSandboxUnavailable instead of silently running locally.
	Sandbox SandboxRunner
	// Ledger receives one cost entry per run (local: session CostTotals
	// delta; sandbox: receipt). nil = no ledger.
	Ledger CostLedger
}

// CostLedger is the append-only sink cron writes run costs to; satisfied by
// *costledger.Store, whose nil receiver reports Enabled()==false.
type CostLedger interface {
	Enabled() bool
	Append(costledger.Entry) bool
}
