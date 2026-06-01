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

import "github.com/naozhi/naozhi/internal/dashboard/cronview"

// CronView is the consolidated narrow consumer interface — see
// docs/design/server-consumer-contracts.md. Aliased to the canonical
// definition so server and dashboard/session share one shape.
// *cron.Scheduler satisfies it implicitly.
type CronView = cronview.CronView
