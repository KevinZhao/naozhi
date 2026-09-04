// Package server — consumer.go: per-consumer interface subsets of
// *session.Router (HubRouter / ScratchRouter / SendRouter) and *Hub
// (HubBroadcaster). Declared here (not in session) so tests can inject fakes;
// the concrete types satisfy them structurally, guarded by
// consumer_contract_test.go. See docs/rfc/consumer-interfaces.md §3.2.2.
package server

import (
	"context"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

// HubRouter is the subset of *session.Router that *Hub consumes on the
// WebSocket subscribe / send / interrupt paths, plus the h.hub.router.*
// transits *ScratchHandler / *SendHandler borrow. Rethink the shape if it
// grows past 15 methods (docs/rfc/consumer-interfaces.md §7.2).
// *session.Router satisfies it structurally; consumer_contract_test.go
// guards the binding.
type HubRouter interface {
	GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error)
	SessionFor(key string) *session.ManagedSession
	Remove(key string) bool
	RenameSession(oldKey, newKey string) bool
	ResetAndDiscardOverride(key string)
	Workspace(chatKey string) string
	SetWorkspace(chatKey, path string)
	SetSessionBackend(key, backend string)
	SetSessionAccessProfile(key, profile string)
	DefaultWorkspace() string
	RegisterForResume(key, sessionID, workspace, lastPrompt string) (effectiveKey string)
	InterruptSession(key string) bool
	InterruptSessionSafe(key string) session.InterruptOutcome
	InterruptSessionViaControl(key string) session.InterruptOutcome
	NotifyIdle()
}

// ScratchRouter is the *ScratchHandler-only subset of *session.Router
// (#566). *session.Router satisfies it structurally; tests inject a fake
// via ScratchHandler.router.
type ScratchRouter interface {
	SessionFor(key string) *session.ManagedSession
	Remove(key string) bool
	RenameSession(oldKey, newKey string) bool
}

// SendRouter is the *SendHandler-only subset of *session.Router, used by
// resolveAttachmentWorkspace (#566).
type SendRouter interface {
	SessionFor(key string) *session.ManagedSession
	Workspace(chatKey string) string
}

// HubBroadcaster names the broadcast / fan-out facet of *Hub — the "push a
// frame to authenticated WS clients" surface producers (router SetOnChange,
// send paths, cron / sysession run-lifecycle hooks, node register/deregister)
// reach for. *Hub satisfies it structurally; consumer_contract_test.go guards
// the binding. Prefer the narrower subsets (SessionsBus.Publish,
// scratch.Broadcaster) when only part of this surface is needed.
type HubBroadcaster interface {
	BroadcastSessionReady(key string)
	BroadcastSessionsUpdate()
	BroadcastCronRunStarted(jobID, runID string, startedAt time.Time, trigger, sessionID string, fresh bool)
	BroadcastCronRunEnded(jobID, runID, state string, startedAt, endedAt time.Time, durationMS int64, sessionID, errClass, errMsg, trigger string)
	BroadcastDaemonRunStarted(name, runID, trigger string, startedAt time.Time)
	BroadcastDaemonRunEnded(name, runID, state, errClass, trigger string, durationMS int64)
}
