// CronView is a type alias for the canonical definition in the leaf package
// internal/dashboard/cronview, shared with internal/dashboard/session (#1536).

package server

import (
	"github.com/naozhi/naozhi/internal/dashboard/cronview"
)

// CronView is the consolidated narrow consumer interface — see
// docs/design/server-consumer-contracts.md. Aliased to the canonical
// definition so server and dashboard/session share one shape.
// *cron.Scheduler satisfies it implicitly.
type CronView = cronview.CronView

// cronScheduler is the server-package consumer view of *cron.Scheduler: the
// two interfaces the value is forwarded into. *cron.Scheduler satisfies it
// implicitly (pinned by cronview_contract_test.go) (#1648).
type cronScheduler interface {
	cronview.CronView
	cronCommandScheduler
}
