// File eventlog_loadbefore.go: the HistorySource adapter over the ring's
// read path.

package cli

import (
	"context"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// LoadBefore adapts the ring to the HistorySource pagination contract: up to
// `limit` entries strictly older than beforeMS, oldest → newest. 语义与
// EntriesBefore 逐字一致（beforeMS<=0 视为无上界、limit<=0 返回空），与
// naozhilog.Source.LoadBefore 对齐，两 tier 的分页结果可以直接拼接。
//
// The ring never blocks, so ctx is unused and the error is always nil; the
// signature satisfies cli.HistorySource（= eventlog/api.Reader）so *EventLog
// implements api.EventStore without a wrapper（守卫见
// internal/eventlog/api/api_assert_test.go）。
func (l *EventLog) LoadBefore(_ context.Context, beforeMS int64, limit int) ([]clievent.EventEntry, error) {
	return l.EntriesBefore(beforeMS, limit), nil
}
