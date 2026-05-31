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
	"time"

	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/wshub"
)

// HubRouter is a type alias for wshub.HubRouter.
//
// Phase 4b-router 搬迁（2026-05-28）：完整接口定义已搬到
// internal/wshub/types.go；server 包用 alias 保持向后兼容，所有 *Server
// 字段 / *Hub 字段 / handler 字段 / mock 都不需要改 import 路径。Phase 4b
// 后续刀（subscribe/broadcast/send 方法搬迁）+ Phase 4 全部完成后，
// 本 alias 可移除（届时 server 包直接 import wshub.HubRouter）。
//
// 历史 godoc（pre-Phase 4b-router）：
//
// HubRouter is the *Hub-only subset of *session.Router. Method list =
// direct h.router. calls (wshub*.go / send.go) PLUS dashboard_scratch.go
// / dashboard_send.go's h.hub.router.* transits where *ScratchHandler /
// *SendHandler intentionally borrow Hub's router handle. 14 methods.
// See docs/rfc/consumer-interfaces.md §3.2.2.
type HubRouter = wshub.HubRouter

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
	GetSession(key string) *session.ManagedSession
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
	GetSession(key string) *session.ManagedSession
	GetWorkspace(chatKey string) string
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
