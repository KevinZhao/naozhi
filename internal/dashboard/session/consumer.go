// consumer.go — the consumer-side dependency interfaces this package needs
// (#2561).
//
// Deps used to name concrete types: *session.Router (77 exported methods),
// *project.Manager, *node.CacheManager, *discovery.RetiredStore. The physical
// split worked — no dashboard sub-package imports internal/server — but taking
// the whole type back means this package is coupled to every future method on
// it, and a test that wants a fake has to build the real thing.
//
// Each interface below is the measured call surface, not a guess: grep for
// h.<field>.<Method> across the package. Three of the four are tiny, which is
// the interesting part — this package needed 2 methods of *project.Manager and 2
// of *node.CacheManager.
//
// Declared here rather than in internal/dashboard/contracts because these are
// per-consumer shapes: dashproject and dashcron use different subsets of the
// same Router, and a shared interface would be the union, i.e. back to a wide
// type. contracts is for shapes several sub-packages genuinely share
// (IPLimiter). This follows docs/rfc/consumer-interfaces.md §3.1, "accept
// interfaces where they are used".
package session

import (
	"context"
	"time"

	"github.com/naozhi/naozhi/internal/project"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/runhistory"
)

// RouterView is the 18 *session.Router methods this package calls, out of 77
// exported. *session.Router satisfies it structurally; the compile-time
// assertion lives at the wiring site (internal/server).
//
// Still 18 and not the ≤6 the issue guessed: this package serves
// /api/sessions, which is the list, the per-session detail, the run history,
// the label/tuning writes and the interrupt — the Router really is its
// datasource. What the narrowing buys is that the other 59 methods can change
// without touching this package.
type RouterView interface {
	// Session list + change detection (the 1 Hz dashboard poll).
	ListSessionsWithVersion() ([]sessionpkg.SessionSnapshot, uint64)
	ListSessionsIfChanged(sinceVersion uint64) (snapshots []sessionpkg.SessionSnapshot, version uint64, changed bool)
	BumpVersion()
	Stats() (active, total int)
	SessionFor(key string) *sessionpkg.ManagedSession

	// Run history.
	SessionRuns(key string, limit int, before time.Time) []runhistory.SessionRun
	SessionRunStats(key string) runhistory.SessionRunStats

	// Writes reachable from the session list UI.
	SetUserLabel(key, label string) bool
	SetSessionTuning(ctx context.Context, key string, model, effort *string) (string, error)
	InterruptSessionSafe(key string) sessionpkg.InterruptOutcome
	RemoveAsync(key string) bool
	RegisterForResume(key, sessionID, workspace, lastPrompt string) (effectiveKey string)

	// Static facts folded into the stats payload.
	CLIName() string
	CLIVersion() string
	MaxProcs() int
	DefaultWorkspace() string
	Workspace(chatKey string) string
	DiscoveryExcludeIDs() map[string]bool
}

// ProjectSource is the *project.Manager surface this package uses: two methods,
// for the stats.projects block.
type ProjectSource interface {
	All() []*project.Project
	ResolveWorkspaces(paths []string) map[string]string
}

// NodeCacheReader is the *node.CacheManager surface: the two cached read paths
// that fold remote-node sessions and projects into the local payload.
type NodeCacheReader interface {
	Sessions() (map[string][]map[string]any, map[string]string)
	Projects() map[string][]map[string]any
}

// RetiredReader is the *discovery.RetiredStore surface: the retired-session
// ledger behind last-active sorting. Nil disables it (the store is optional
// when StateDir is unset), so callers keep their nil checks.
type RetiredReader interface {
	Snapshot() map[string]int64
	MarkRetired(sessionID string, now time.Time)
	Prune(cutoffMs int64) int
	Save() error
}
