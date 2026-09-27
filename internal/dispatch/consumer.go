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
	"github.com/naozhi/naozhi/internal/projectapi"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// Session is what the dispatcher reads off the session the router hands it:
// the backend, for the reply footer. The value otherwise travels unchanged
// to Capabilities.Send, and the host that produced it (server) reads the
// rest.
type Session interface {
	Backend() string
}

// KeyResolver is the *session.KeyResolver surface the dispatcher uses: the
// routed key and opts for a chat, the bare key, and the chat's project
// binding.
type KeyResolver interface {
	ResolveForChat(platform, chatType, chatID, agentID string) (key string, opts sessionview.AgentOpts)
	KeyForChat(platform, chatType, chatID, agentID string) string
	ProjectBindingForChat(platform, chatType, chatID string) projectapi.ProjectBinding
}

// SessionRouter is the subset of *session.Router that Dispatcher uses. The
// router's own GetOrCreate returns the concrete session, so production passes
// an adapter (server's dispatchRouter); a missing session must arrive as a
// nil interface.
// Adding a new Router call from dispatch requires extending this interface —
// kept small so growth is visible in review.
type SessionRouter interface {
	GetOrCreate(ctx context.Context, key string, opts sessionview.AgentOpts) (Session, sessionview.SessionStatus, error)
	// DiscardPassthroughPending clears in-flight passthrough sends for the
	// keyed session (no-op when absent). Routed through the interface so
	// discardQueue never touches the session itself (#1612).
	DiscardPassthroughPending(key string, reason error)
	Reset(key string)
	Workspace(chatKey string) string
	SetWorkspace(chatKey, path string)
	// ResetChatAndSetWorkspace atomically resets the chat and installs a new
	// workspace override (#2342) — used by /cd to avoid the reset/set race.
	ResetChatAndSetWorkspace(chatKeyPrefix, path string)
	InterruptSessionViaControl(key string) sessionview.InterruptOutcome
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
