// Package cronview holds the single canonical definition of the narrow
// consumer interface that internal/server and internal/dashboard/session both
// need from *cron.Scheduler. It lives in this leaf package (which imports
// nothing internal) so both sides can import it without a cycle (#1536), and
// stays out of the cron package so neither consumer couples to cron's full
// Scheduler API. *cron.Scheduler satisfies CronView implicitly.
package cronview

// CronView is the narrow consumer interface over *cron.Scheduler shared by the
// server and dashboard/session packages (#754). See
// docs/design/server-consumer-contracts.md.
//
// EnsureStub returns false when (a) the key isn't a `cron:` key (legitimate
// no-op), (b) the cron job ID is unknown to the scheduler (callers fall through
// to the nil-session 404), or (c) stub registration failed inside cron (slog'd
// there). The bool-only contract is behaviourally sufficient (#772).
//
// KnownSessionIDs returns a READ-ONLY set of cron-spawned Claude session IDs;
// consumers must not mutate or persist it (#1544).
type CronView interface {
	EnsureStub(key string) bool
	SetJobPrompt(jobID, prompt string) error
	KnownSessionIDs() map[string]struct{}
}
