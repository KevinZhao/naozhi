// Phase 3e (server-split-phase4-design.md §6.5 Plan B): CronView previously
// lived in dashboard_session.go. After SessionHandlers moved to
// internal/dashboard/session, this server-package copy keeps wshub.go +
// cronview_contract_test.go call sites compiling without reverse-importing
// the sub-package.
//
// The two definitions stay structurally identical; *cron.Scheduler
// satisfies both.

package server

// CronView is the consolidated narrow consumer interface — see
// docs/design/server-consumer-contracts.md.
type CronView interface {
	EnsureStub(key string) bool
	SetJobPrompt(jobID, prompt string) error
	KnownSessionIDs() map[string]bool
}
