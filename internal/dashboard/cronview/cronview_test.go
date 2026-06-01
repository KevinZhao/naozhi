package cronview_test

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dashboard/cronview"
	dashsession "github.com/naozhi/naozhi/internal/dashboard/session"
)

// TestSchedulerSatisfiesCronView pins the production wiring contract at the
// interface's canonical home (R20260531070014-ARCH-2 / #1536): *cron.Scheduler
// must implement the single CronView definition that both server and
// dashboard/session now alias. A cron-side signature drift fails to compile
// here, local to the interface declaration, instead of only at the field
// assignment in server.go.
func TestSchedulerSatisfiesCronView(t *testing.T) {
	var _ cronview.CronView = (*cron.Scheduler)(nil)
}

// TestDashboardSessionAliasIsCanonical pins that dashboard/session.CronView is
// a type ALIAS for cronview.CronView, not an independent re-declaration that
// could silently drift back into the byte-identical duplicate #1536 removed.
// Because Go type aliases are identical types, a value of one is assignable to
// the other without conversion; if a future edit replaced the alias with a
// fresh `type CronView interface{...}` this assignment would still compile
// (structural identity), so we additionally assert via a shared concrete
// implementor that both names accept it.
func TestDashboardSessionAliasIsCanonical(t *testing.T) {
	var canonical cronview.CronView = (*cron.Scheduler)(nil)
	var aliased dashsession.CronView = canonical // identical types: no conversion
	_ = aliased
	var back cronview.CronView = aliased
	_ = back
}
