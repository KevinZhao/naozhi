// Package server — consumer.go (HubRouter / ScratchRouter / SendRouter)
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
// See docs/rfc/consumer-interfaces.md §3.2.2.
package server

import (
	"context"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

// HubRouter is the subset of *session.Router that *Hub consumes on the
// WebSocket subscribe / send / interrupt paths. Method list = direct
// h.router. calls (wshub*.go / send.go) PLUS dashboard_scratch.go /
// dashboard_send.go's h.hub.router.* transits where *ScratchHandler /
// *SendHandler intentionally borrow Hub's router handle. 14 methods —
// under the "rethink at >15" threshold from
// docs/rfc/consumer-interfaces.md §7.2.
//
// *session.Router satisfies this implicitly via Go structural typing,
// guarded at CI time by consumer_contract_test.go.
//
// G1 (#2195, docs/rfc/godstruct-extraction.md): this declaration was
// pulled back here from the internal/wshub leaf package, which held only
// a stale Phase-4a interface skeleton (the 49-field Hub mirror was
// deleted in #1741) plus five interfaces with zero live consumers. The
// `type HubRouter = wshub.HubRouter` alias and the whole wshub package
// are removed in the same change.
type HubRouter interface {
	GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error)
	SessionFor(key string) *session.ManagedSession
	Remove(key string) bool
	RenameSession(oldKey, newKey string) bool
	ResetAndDiscardOverride(key string)
	Workspace(chatKey string) string
	SetWorkspace(chatKey, path string)
	SetSessionBackend(key, backend string)
	DefaultWorkspace() string
	RegisterForResume(key, sessionID, workspace, lastPrompt string) (effectiveKey string)
	InterruptSession(key string) bool
	InterruptSessionSafe(key string) session.InterruptOutcome
	InterruptSessionViaControl(key string) session.InterruptOutcome
	NotifyIdle()
}

// ScratchRouter is the *ScratchHandler-only subset of *session.Router.
// Closes the Phase 2.5 cleanup item flagged in the consumer.go godoc
// header above: dashboard_scratch.go now reaches its router via this
// interface instead of borrowing *Hub's concrete router handle as a
// transit. R215-ARCH-P1-4 / #566.
//
// *session.Router satisfies the interface implicitly via Go structural
// typing — no contract test wiring is required because *session.Router
// already exposes these three methods (each call site previously typed
// h.hub.router.X). Tests can inject a fake by assigning ScratchHandler.
// router to a local stub.
type ScratchRouter interface {
	SessionFor(key string) *session.ManagedSession
	Remove(key string) bool
	RenameSession(oldKey, newKey string) bool
}

// SendRouter is the *SendHandler-only subset of *session.Router.
// Same Phase 2.5 cleanup as ScratchRouter: dashboard_send.go's two
// h.hub.router.* call sites for resolveAttachmentWorkspace now flow
// through this interface so the handler's router dependency is
// declared at its own type rather than borrowed off *Hub.
// R215-ARCH-P1-4 / #566.
type SendRouter interface {
	SessionFor(key string) *session.ManagedSession
	Workspace(chatKey string) string
}

// HubBroadcaster names the broadcast / fan-out facet of *Hub — the
// "push a frame to authenticated WS clients" surface that producers
// (router SetOnChange, send paths, cron / sysession run-lifecycle hooks,
// node register/deregister) reach for, distinct from the connection-pool
// and subscribe/send machinery the rest of the Hub owns.
//
// R237-ARCH-10: the Hub mixes ConnPool / Broadcaster / SendPath /
// AgentLinker concerns across 22+ fields. A field-level struct split is
// gated by the wshub.go field-block lint contract (no field reordering),
// so this is the additive first slice: a *named consumer interface* that
// pins the Broadcaster facet's public method set. It lets the broadcast
// surface be reasoned about, faked in tests, and eventually carved onto a
// dedicated sub-struct without touching call sites today. *Hub satisfies
// it structurally; consumer_contract_test.go guards the binding so a
// signature drift breaks the build.
//
// Narrower per-consumer subsets already exist and remain the preferred
// dependency for code that only needs part of this surface
// (SessionsBus.Publish wraps BroadcastSessionsUpdate; scratch.Broadcaster
// is the single-method nudge). HubBroadcaster is the full producer-facing
// facet, not a replacement for those.
type HubBroadcaster interface {
	BroadcastSessionReady(key string)
	BroadcastSessionsUpdate()
	BroadcastCronRunStarted(jobID, runID string, startedAt time.Time, trigger, sessionID string, fresh bool)
	BroadcastCronRunEnded(jobID, runID, state string, startedAt, endedAt time.Time, durationMS int64, sessionID, errClass, errMsg, trigger string)
	BroadcastDaemonRunStarted(name, runID, trigger string, startedAt time.Time)
	BroadcastDaemonRunEnded(name, runID, state, errClass, trigger string, durationMS int64)
}
