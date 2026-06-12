package cron

// deps.go declares SchedulerDeps, the dependency half of the cfg/deps split
// ratified in docs/rfc/cron-sysession-merge.md §3.5.1 (#746, R238-ARCH-5).
//
// Boundary rule (§3.5.1): a field whose type is an interface, a func, or a
// map of components belongs in SchedulerDeps; scalar / value configuration
// (numbers, durations, paths, TZ, NotifyTarget-style value structs) stays in
// SchedulerConfig. Exception H3: context.Context is treated as a lifecycle
// scalar — it expresses "parent lifetime", not a component contract — so
// ParentCtx stays on SchedulerConfig (same convention as
// http.Server.BaseContext). AllowNilRouter is a plain bool switch and stays
// on SchedulerConfig even though it talks *about* a dependency.
//
// The split is part of the API contract and is pinned by
// deps_boundary_test.go.

import "github.com/naozhi/naozhi/internal/runtelemetry"

// SchedulerDeps carries the injected components the cron Scheduler talks to.
// All fields are idiomatic-optional (nil/empty = feature off) except Router,
// which is required in production — see the SchedulerConfig docstring and
// the AllowNilRouter escape hatch for the boot-time contract.
type SchedulerDeps struct {
	// Router is the session router the scheduler talks to. Accepts the
	// SessionRouter interface so tests can pass a minimal fake; production
	// passes a *session.Router which satisfies it transparently.
	Router SessionRouter
	// NotifySender resolves a platform name to its PlatformReplier for cron
	// completion notices. #725: replaces the former
	// Platforms map[string]platform.Platform so internal/cron no longer
	// imports internal/platform — the wireup layer builds a
	// platformNotifySender adapter over the live platform map. nil = no
	// notify delivery (the Lookup miss path keeps notifyTarget's existing
	// "platform not found" WARN).
	NotifySender  NotifySender
	Agents        map[string]AgentOpts
	AgentCommands map[string]string
	// Telemetry receives RunStartedEvent / RunEndedEvent for every cron
	// run via the shared runtelemetry shape. nil = no broadcast (tests /
	// no-WS deployments). Replaces the legacy SetOnRunStarted /
	// SetOnRunEnded / SetOnExecute setter trio. Late injection is also
	// supported via SetTelemetry — cmd/naozhi builds the Scheduler before
	// the Hub exists, then wires the broadcaster from dashboard.go; this
	// field is the construction-time injection seam for tests and simple
	// wirings, and the two paths coexist. (RFC §3.5)
	Telemetry runtelemetry.Broadcaster
	// Sandbox executes placement=sandbox jobs on AgentCore microVMs
	// (agentcore-cloud-sandbox RFC §4.2). The wireup layer builds it over
	// internal/agentcore so cron never imports the AWS SDK. nil = sandbox
	// placement unavailable; such jobs terminate with
	// ErrClassCronSandboxUnavailable instead of silently running locally.
	Sandbox SandboxRunner
}
