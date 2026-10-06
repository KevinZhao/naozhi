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
	"github.com/naozhi/naozhi/internal/turn"
)

// KeyResolver is the *session.KeyResolver surface the dispatcher uses: the
// routed key and opts for a chat, the bare key, and the chat's project
// binding.
type KeyResolver interface {
	ResolveForChat(platform, chatType, chatID, agentID string) (key string, opts sessionview.AgentOpts)
	KeyForChat(platform, chatType, chatID, agentID string) string
	ProjectBindingForChat(platform, chatType, chatID string) projectapi.ProjectBinding
}

// SessionRouter is the subset of *session.Router that Dispatcher's slash
// commands use; turns go through Turns. *session.Router satisfies it.
// Adding a new Router call from dispatch requires extending this interface —
// kept small so growth is visible in review.
type SessionRouter interface {
	Workspace(chatKey string) string
	// ResetChatAndSetWorkspace atomically resets the chat and installs a new
	// workspace override (#2342) — used by /cd to avoid the reset/set race.
	ResetChatAndSetWorkspace(chatKeyPrefix, path string)
	InterruptSessionViaControl(key string) sessionview.InterruptOutcome
	// The /model, /effort and /backend surface (commands_tuning.go).
	// SetSessionTuning returns the sessionview.TuningApplied* mode taken;
	// VisitSessions is how a command reads one key's snapshot back.
	SetSessionTuning(ctx context.Context, key string, model, effort *string) (string, error)
	SetSessionBackend(key, backend string)
	VisitSessions(fn func(sessionview.SessionSnapshot) bool)
}

// Turns is the *turn.Orchestrator surface the dispatcher submits IM turns
// through: every message, /urgent, and the /new and /clear resets.
type Turns interface {
	Submit(ctx context.Context, r turn.Request, a turn.Admission) turn.Ack
	Reset(ctx context.Context, key string, discardOverride bool)
	// ShouldNotify rate-limits the queued and busy text notices per key.
	ShouldNotify(key string) bool
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
