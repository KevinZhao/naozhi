// File eventlog_loadbefore.go: the HistorySource adapter over the ring's
// read path. Split from the query file per docs/rfc/eventlog-split.md
// one-concern-per-file 惯例；方法本身是 docs/rfc/eventlog-subsystem-unify.md
// §8 Phase 1 的 thin-adapter 预案落地——补齐 ring 相对 api.EventStore
// 的唯一方法缺口。

package cli

import (
	"context"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// LoadBefore adapts the ring to the canonical HistorySource pagination
// contract: up to `limit` entries strictly older than beforeMS, oldest →
// newest. 语义与 EntriesBefore 逐字一致（beforeMS<=0 视为无上界、
// limit<=0 返回空）——与 naozhilog.Source.LoadBefore 的边界行为对齐，
// 两 tier 的分页结果可以直接拼接。
//
// The ring is in-memory and never blocks, so ctx is intentionally unused
// and the returned error is always nil; the signature exists to satisfy
// cli.HistorySource（= eventlog/api.Reader），让 *EventLog 无需 wrapper
// struct 即满足 api.EventStore。编译期守卫见
// internal/eventlog/api/api_assert_test.go。
func (l *EventLog) LoadBefore(_ context.Context, beforeMS int64, limit int) ([]clievent.EventEntry, error) {
	return l.EntriesBefore(beforeMS, limit), nil
}
