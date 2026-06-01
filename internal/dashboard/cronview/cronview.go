// Package cronview holds the single canonical definition of the narrow
// consumer interface that the server package and the dashboard/session
// sub-package both need from *cron.Scheduler.
//
// R20260531070014-ARCH-2 (#1536): the 3-method CronView interface was
// declared byte-identically in two places — internal/server/cronview.go and
// internal/dashboard/session/handlers.go. The duplication existed only to
// dodge a reverse import after SessionHandlers was split out of the server
// package (server imports dashboard/session, so dashboard/session cannot
// import server). Hoisting the definition into this leaf package — which
// imports nothing internal and is therefore importable from both sides
// without a cycle — lets both call sites reference one shape via a type
// alias, mirroring the `HubRouter = wshub.HubRouter` pattern already used in
// internal/server/consumer.go.
//
// *cron.Scheduler satisfies CronView implicitly. The interface stays in a
// dedicated leaf rather than in the cron package so neither consumer couples
// to cron's full Scheduler API — the whole point of the narrow interface.
package cronview

// CronView is the consolidated narrow consumer interface. R242-ARCH-13
// (#754) collapsed three previously-separate single-method shapes —
// cronHubOps (EnsureStub + SetJobPrompt), cronStubChecker (EnsureStub) and
// cronSessionLister (KnownSessionIDs) — into one interface so reviewers and
// test authors only have to learn one shape. See
// docs/design/server-consumer-contracts.md.
//
// EnsureStub returns false in three cases callers historically had to
// disambiguate by side-effect: (a) the key isn't a `cron:` key (legitimate
// no-op); (b) the parsed cron job ID is unknown to the scheduler (job
// deleted before the dashboard tab re-subscribed — callers fall through to
// the nil-session 404, which is correct); (c) the job is known but stub
// registration failed inside cron (rare; slog'd by the cron implementation).
// Promoting EnsureStub to (ok bool, reason string) is queued behind the
// cron→server interface tightening RFC because it breaks every test mock and
// the *cron.Scheduler concrete signature; the bool-only contract is already
// behaviourally correct. R242-ARCH-28 (#772).
type CronView interface {
	EnsureStub(key string) bool
	SetJobPrompt(jobID, prompt string) error
	KnownSessionIDs() map[string]bool
}
