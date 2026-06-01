// Package dispatch — consumer.go
//
// SessionRouter is the consumer-side interface that Dispatcher relies
// on for router operations. Declared here (not in session) so:
//   - session.Router can evolve without cascading breakage across the
//     three consumer packages (dispatch / server / upstream);
//   - Dispatcher tests can inject a fake without wiring a full router
//     graph (cli.Wrapper, shim.Manager, eventLogPersister, tmp
//     workspace, etc.).
//
// *session.Router satisfies this interface implicitly via Go structural
// typing. An editorial drift (e.g. Router adds an argument to
// GetOrCreate) is caught at compile time by
// internal/session/contract_test.go where `var _ dispatch.SessionRouter
// = (*session.Router)(nil)` acts as a cross-package pin.
//
// Design: single interface per consumer (cron.SessionRouter is the
// existing precedent). See docs/rfc/consumer-interfaces.md §3.4 for
// why we do NOT split into Lifecycle/Reader/Controller sub-interfaces
// at this size (8 methods). The "as long as it stays small" caveat is
// machine-enforced by consumer_method_count_test.go: growth past 8
// methods trips a test that points back at the Lookup/Lifecycle/Mutator
// split tracked in R246-ARCH-11 (#791).
package dispatch

import (
	"context"

	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// SessionRouter is the subset of *session.Router that Dispatcher uses.
// Method list is derived from `grep 'd\.router\.' internal/dispatch/`
// (13 d.router.* call sites, dedup to 8 distinct methods). Adding a
// new Router call from dispatch requires extending this interface —
// kept small so growth is visible in review.
type SessionRouter interface {
	GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error)
	GetSession(key string) *session.ManagedSession
	Reset(key string)
	ResetChat(chatKeyPrefix string)
	GetWorkspace(chatKey string) string
	SetWorkspace(chatKey, path string)
	InterruptSessionViaControl(key string) session.InterruptOutcome
	NotifyIdle()
}

// ProjectStore is the subset of *project.Manager that Dispatcher's slash-
// command handlers use (/project, /cd, /new project-echo). Declared here
// (ARCH-DISP-1, #457) so the slash-command tests can inject a fake binding
// store without standing up a real project.Manager (projects.root dir +
// on-disk binding file). *project.Manager satisfies this implicitly via
// structural typing; internal/project/contract_test.go (or the
// compile-time pin in NewDispatcher) catches signature drift.
//
// Method list derived from `grep 'd\.projectMgr\.' internal/dispatch/`
// (5 distinct methods). Adding a new projectMgr call from dispatch
// requires extending this interface, keeping growth visible in review.
//
// Return types stay *project.Project / []*project.Project — the value
// type, not the manager — so the handlers keep reading proj.Name /
// proj.Path. Mirrors SessionRouter returning session.AgentOpts: the
// decoupling that matters is the manager method set, not the leaf value.
type ProjectStore interface {
	Get(name string) *project.Project
	All() []*project.Project
	ProjectForChat(platform, chatType, chatID string) *project.Project
	BindChat(projectName, platform, chatType, chatID string) error
	UnbindAllChat(platform, chatType, chatID string) error
}
