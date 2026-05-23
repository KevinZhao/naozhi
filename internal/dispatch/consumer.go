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
// at this size (8 methods).
package dispatch

import (
	"context"

	"github.com/naozhi/naozhi/internal/session"
)

// SessionRouter is the subset of *session.Router that Dispatcher uses.
// Method list is derived from `grep 'd\.router\.' internal/dispatch/`
// (13 d.router.* call sites, dedup to 8 distinct methods). Adding a
// new Router call from dispatch requires extending this interface —
// kept small so growth is visible in review.
//
// Why returns *session.ManagedSession rather than a narrow handle
// (R234-ARCH-17, deliberate):
// dispatch needs a handful of ManagedSession methods after GetOrCreate /
// GetSession (Send, Key, BackendTag, queue depth, etc.) plus the ability
// to pass the session into other consumer-side helpers that already type
// the parameter as *session.ManagedSession. Introducing a SessionHandle
// interface here would either (a) replicate ~10 method signatures that
// must drift in lockstep with ManagedSession or (b) force every caller
// in dispatch to do the type-assertion dance. The compile-time pin in
// internal/session/contract_test.go already catches editorial drift on
// the eight methods this interface declares; widening the surface to
// the full struct is the lower-cost choice until session-package boundary
// work (R230-ARCH-3 / R234-ARCH-3) lands a unified RouterView DTO.
//
// Do not add new methods that take *session.ManagedSession as a parameter
// to dispatch helpers; instead, accept the narrowest interface the helper
// actually needs (`interface { Send(...) ... }`). That lets the test fake
// in dispatch_test.go shrink rather than grow when surface widens.
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
