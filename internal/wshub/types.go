// Package wshub holds the consumer interfaces the WebSocket Hub depends on
// (server-split-phase4-design v0.6.1 §6.5).
//
// 现状：本包只保留接口契约。HubRouter 等接口有真实消费者——
// internal/server/consumer.go 通过 `type HubRouter = wshub.HubRouter`
// alias 引用，dispatch 侧 var _ 绑定 MessageEnqueuer。
//
// 历史：Phase 4a 曾在本包落地一个 49 字段的 server.Hub 镜像骨架
// (Hub struct + NewHub/Shutdown + placeholder 方法 + 字段计数测试)，
// 但 Phase 4b cutover 从未发生、无任何生产 instantiation，已于 #1741
// 删除（同 #1600 删零消费者骨架的模式）。生产 Hub 实现仍在
// internal/server/wshub*.go。
package wshub

import (
	"context"
	"net/http"
	"time"

	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/session"
)

// MessageEnqueuer is the *dispatch.MessageQueue subset Hub depends on for
// the dashboard-side write path.
//
// satisfies-by: *dispatch.MessageQueue (internal/dispatch/msgqueue.go)
//
// Method set 与 server.MessageEnqueuer 完全一致——Phase 4b 起 server 包
// 的 wshub_types.go 删除，dispatch 侧 var _ 切到本接口。
type MessageEnqueuer interface {
	Enqueue(key string, msg dispatch.QueuedMsg) (isOwner, enqueued, shouldInterrupt bool, gen uint64)
	DoneOrDrain(key string, gen uint64) []dispatch.QueuedMsg
	Discard(key string)
	Mode() dispatch.QueueMode
	CollectDelay() time.Duration
}

// HubRouter is the *session.Router subset *Hub consumes on the WebSocket
// subscribe / send / interrupt paths.
//
// satisfies-by: *session.Router (implicitly via Go structural typing;
// guarded at CI time by internal/session/contract_test.go).
//
// Method list = direct h.router.* calls in wshub*.go and send.go. 14
// methods total — under the "rethink at >15" threshold from
// docs/rfc/consumer-interfaces.md §7.2.
//
// Phase 4b-router 搬迁（2026-05-28）：本接口从 internal/server/consumer.go
// 完整搬到本包。server.HubRouter 改为 type alias `= wshub.HubRouter` 保持
// 向后兼容，server 包内的 *Server 字段 / handler 字段类型不需要改。
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

// CronView is the cron.Scheduler subset Hub uses for cron stub revival
// (R232-ARCH-7). Phase 4b 起从 server.CronView 同步。
type CronView interface {
	HasJob(id string) bool
}

// ScratchOps is the *session.ScratchPool subset Hub uses for ephemeral
// session opts derivation. Phase 4b 起从 server 包搬过来。
type ScratchOps interface {
	OptsForKey(key string) (any, bool)
}

// UploadOps is the *uploadStore subset Hub uses for resolving WS-sent
// file_ids. Phase 4b 起从 server 包搬过来。
type UploadOps interface {
	Resolve(fileID string) (path string, ok bool)
}

// Auth is the AuthHandlers subset Hub uses for nz_anon cookie minting
// (R20260527122801-SEC-2 / #1326). Phase 4b 起从 server 包搬过来。
type Auth interface {
	MintAnonCookie(w http.ResponseWriter, r *http.Request) string
}
