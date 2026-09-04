// Package dispatch — consumer.go
//
// SessionRouter is the consumer-side interface Dispatcher relies on for
// router operations. Declared here (not in session) so session.Router can
// evolve without cascading breakage across consumer packages and Dispatcher
// tests can inject a fake without a full router graph. *session.Router
// satisfies it implicitly; internal/session/contract_test.go pins the
// contract at compile time. One interface per consumer by design — see
// docs/rfc/consumer-interfaces.md §3.4.
package dispatch

import (
	"context"

	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// SessionRouter is the subset of *session.Router that Dispatcher uses.
// Adding a new Router call from dispatch requires extending this interface —
// kept small so growth is visible in review.
type SessionRouter interface {
	GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error)
	// DiscardPassthroughPending clears in-flight passthrough sends for the
	// keyed session (no-op when absent). Routed through the interface so
	// discardQueue never touches the concrete *session.ManagedSession (#1612).
	DiscardPassthroughPending(key string, reason error)
	Reset(key string)
	ResetChat(chatKeyPrefix string)
	Workspace(chatKey string) string
	SetWorkspace(chatKey, path string)
	// ResetChatAndSetWorkspace atomically resets the chat and installs a new
	// workspace override (#2342) — used by /cd to avoid the reset/set race.
	ResetChatAndSetWorkspace(chatKeyPrefix, path string)
	InterruptSessionViaControl(key string) session.InterruptOutcome
	NotifyIdle()
}

// ProjectStore is the subset of *project.Manager that Dispatcher's slash-
// command handlers use (/project, /cd, /new project-echo), so tests can
// inject a fake binding store (#457). *project.Manager satisfies it
// implicitly; internal/session/contract_test.go pins the contract. Return
// types stay *project.Project — the decoupling that matters is the manager
// method set, not the leaf value.
type ProjectStore interface {
	Get(name string) *project.Project
	All() []*project.Project
	ProjectForChat(platform, chatType, chatID string) *project.Project
	BindChat(projectName, platform, chatType, chatID string) error
	UnbindAllChat(platform, chatType, chatID string) error
}
