// Package server — consumer.go (HubRouter)
//
// HubRouter is the subset of *session.Router that *Hub consumes on the
// WebSocket subscribe / send / interrupt paths. Declared here (not in
// session) so Hub tests can inject a fake without starting a real
// router; *session.Router satisfies the interface implicitly via Go
// structural typing, guarded at CI time by
// internal/session/contract_test.go.
//
// Method list derived from grep 'h\.router\.' restricted to files
// where the *Hub receiver lives (wshub.go / wshub_agent.go / send.go).
// The s.router./h.router. calls in dashboard_session.go,
// project_api.go, health.go, dashboard_cli.go, takeover.go, server.go,
// dashboard.go are on *SessionHandlers / *ProjectHandlers /
// *HealthHandler / *CLIBackendsHandler / *Server receivers; those are
// NOT part of Hub and are deferred to ARCH-SERVER-ROUTER-IF (RFC
// §Phase 2.5, non-goal of the current RFC).
//
// R215-ARCH-P1-4 (#566): *ScratchHandler / *SendHandler now own the
// ScratchRouter / SendRouter consumer interfaces declared below
// instead of borrowing Hub's router handle. Other handlers in the
// "deferred" list above remain unchanged.
//
// See docs/rfc/consumer-interfaces.md §3.2.2.
package server

import (
	"context"

	"github.com/naozhi/naozhi/internal/session"
)

// HubRouter is the *Hub-only subset of *session.Router. Method list
// derived from direct h.router. calls in wshub*.go / send.go.
//
// R215-ARCH-P1-4 (#566): the Phase 2.5 cleanup that the previous godoc
// flagged as out-of-scope has landed for *ScratchHandler / *SendHandler —
// see ScratchRouter / SendRouter below. Their handlers no longer transit
// through h.hub.router; this interface is now genuinely Hub-only.
//
// 14 methods — under the "rethink at >15" threshold from docs/rfc/
// consumer-interfaces.md §7.2.
type HubRouter interface {
	GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error)
	GetSession(key string) *session.ManagedSession
	Remove(key string) bool
	RenameSession(oldKey, newKey string) bool
	ResetAndDiscardOverride(key string)
	GetWorkspace(chatKey string) string
	SetWorkspace(chatKey, path string)
	SetSessionBackend(key, backend string)
	DefaultWorkspace() string
	RegisterForResume(key, sessionID, workspace, lastPrompt string) (effectiveKey string)
	InterruptSession(key string) bool
	InterruptSessionSafe(key string) session.InterruptOutcome
	InterruptSessionViaControl(key string) session.InterruptOutcome
	NotifyIdle()
}

// ScratchRouter is the *ScratchHandler subset of *session.Router. Lifted
// from the previous h.hub.router.* transit so the scratch handler does
// not pay a transitive `*Hub → *Router` import in tests, and so the
// scratch surface stays grep-discoverable on its own. R215-ARCH-P1-4 (#566).
//
// Method list = direct calls in dashboard_scratch.go (handleOpen +
// handlePromote): GetSession at open-time validation; Remove for orphan
// teardown on promote-time defensive paths; RenameSession to atomically
// re-parent a scratch onto a permanent session key.
//
// *session.Router satisfies this interface implicitly via Go structural
// typing; tests can substitute a hand-rolled stub without dragging in
// the full HubRouter surface (14 methods → 3).
type ScratchRouter interface {
	GetSession(key string) *session.ManagedSession
	Remove(key string) bool
	RenameSession(oldKey, newKey string) bool
}

// SendRouter is the *SendHandler subset of *session.Router. Lifted from
// h.hub.router.* in handleAttachment so attachment-resolve does not
// reach across the Hub boundary for what is conceptually a per-key
// workspace lookup. R215-ARCH-P1-4 (#566).
//
// Method list = direct calls in dashboard_send.go's handleAttachment:
// GetSession (live workspace via Snapshot.Workspace) and GetWorkspace
// (paused / discovered fallback via the chat-key → workspace map).
//
// *session.Router satisfies this interface implicitly via Go structural
// typing.
type SendRouter interface {
	GetSession(key string) *session.ManagedSession
	GetWorkspace(chatKey string) string
}
