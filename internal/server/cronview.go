// Phase 3e (server-split-phase4-design.md §6.5 Plan B): CronView previously
// lived in dashboard_session.go. After SessionHandlers moved to
// internal/dashboard/session, a server-package copy kept wshub.go +
// cronview_contract_test.go call sites compiling without reverse-importing
// the sub-package.
//
// R20260531070014-ARCH-2 (#1536): that copy and the byte-identical one in
// internal/dashboard/session/handlers.go are now both type aliases for the
// single canonical definition in the leaf package internal/dashboard/cronview
// — the only consumer that stays independent is wshub/types.go's CronView
// (HasJob only), a genuinely different shape. Mirrors the
// `HubRouter = wshub.HubRouter` alias pattern in consumer.go.

package server

import (
	"github.com/naozhi/naozhi/internal/dashboard/cronview"
	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// CronView is the consolidated narrow consumer interface — see
// docs/design/server-consumer-contracts.md. Aliased to the canonical
// definition so server and dashboard/session share one shape.
// *cron.Scheduler satisfies it implicitly.
type CronView = cronview.CronView

// cronScheduler is the server-package consumer view of *cron.Scheduler
// (R20260603000023-ARCH-2 / #1648). Previously Server.scheduler and
// ServerOptions.Scheduler held the concrete *cron.Scheduler — its full ~60
// method surface — even though the server package only ever forwards the
// value into already-narrowed interface fields (cronCommandScheduler via
// cronDispatchAdapter, cronview.CronView, wshub.CronView) and calls exactly
// one method on it directly: SetTelemetry (routes.go). This aggregate embeds
// the two consumer interfaces the value is forwarded into plus that one
// direct method, so the field type now advertises only what the server
// actually depends on while *cron.Scheduler continues to satisfy it
// implicitly (pinned by cronview_contract_test.go). Mirrors the wshub Hub
// narrowing to CronView.
//
// #1164: the dispatch.CronScheduler embed became the server-local
// cronCommandScheduler (cron_dispatch_adapter.go) when dispatch's seam was
// re-typed to the projection-based CronCommands — dispatch no longer names
// cron types, so the concrete-typed subset now lives on this side.
type cronScheduler interface {
	cronview.CronView
	cronCommandScheduler
	SetTelemetry(b runtelemetry.Broadcaster)
}
