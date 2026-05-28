// Package wshub: WebSocket Hub 抽包目标包（server-split-phase4-design v0.6.1
// §6.5 Phase 4a 骨架）.
//
// Phase 4a 状态：骨架并存。
//   - internal/server/wshub*.go 旧 Hub 仍是生产运行的实现
//   - internal/wshub/ (本包) 是 Phase 4a 落地的骨架——独立 build / test，
//     无外部调用方
//   - Phase 4b 起将 server 包的方法实质搬到本包；server.Hub → wshub.Hub
//     的 import 切换在 Phase 4b PR 一次完成
//
// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     none (interface declarations + var _ binding only)
//	READS:      none
//
// rule 3a/3b 字段块对账对接口文件不适用，但 marker 仍在。
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
