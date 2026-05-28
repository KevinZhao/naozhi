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
	"net/http"
	"time"

	"github.com/naozhi/naozhi/internal/dispatch"
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

// HubRouter is the session.Router subset Hub depends on. Phase 4b 起从
// server.HubRouter 完整搬过来。
type HubRouter interface {
	Version() uint64
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
