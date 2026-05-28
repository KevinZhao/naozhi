// Phase 4c-prep / R-tailer-pools-extract (2026-05-28):
// agent_tailer.go 中两套 sync.Pool（subs 切片池 + buffered 切片池）抽到
// 独立文件。纯物理切分、零行为变化。
//
// 这两套池子构成完整闭环：
//   - tailerSubsPool / tailerSubsHandle / acquire / release（fanout 路径）
//   - tailerBufferedPool / tailerBufferedHandle / acquire / release（attach 路径）
//
// 性能注释（R245-PERF-15 #865 / R249-PERF-4 #926）逐字保留，是日后理解
// "为什么用 pointer-to-slice + 单独 handle 包装" 的唯一来源。
package server

import (
	"sync"

	"github.com/naozhi/naozhi/internal/cli"
)

// tailerSubsPool reuses []*wsClient slices across pollOnce ticks so the
// 200ms event-fanout path stays alloc-free in steady state. R245-PERF-15
// (#865): each pollOnce previously did make([]*wsClient, 0, len(t.subs))
// per tick when there were events to fan out — at 50 active tailers ×
// 5 ticks/s that's 250 GC-visible allocs/s for slices that never escape.
// The pool's New supplies a 4-cap default (matches the typical "1-2
// dashboard tabs subscribed" steady state); larger fan-outs grow the
// underlying slice as usual and the grown slice is then returned to the
// pool, so subsequent ticks with similar subscriber counts skip the
// growth too.
//
// We pool *[]*wsClient (pointer-to-slice) per the sync.Pool best-practice
// note in the standard library — putting the slice header by value means
// every Get/Put cycle allocates a new interface{} header for the slice
// metadata, which would defeat half the pool's purpose. The pointer
// indirection lets the same allocation round-trip end-to-end.
//
// releaseTailerSubsSlice zero-clears the slice so wsClient pointers
// cannot keep clients alive past their unsubscribe — without this, a
// busy tailer's pool would pin one wsClient per parked slot for the
// lifetime of the pool entry.
var tailerSubsPool = sync.Pool{
	New: func() any {
		s := make([]*wsClient, 0, 4)
		return &s
	},
}

// tailerSubsHandle wraps the pool entry pointer so callers can return
// the *exact same* pointer they pulled from Get(). Without this, the
// caller would have to remember the original *[]*wsClient through the
// pollOnce control flow — and writing tailerSubsPool.Put(&local) would
// force `local` to escape to the heap (one alloc per tick), defeating
// the pool's purpose.
type tailerSubsHandle struct {
	sp *[]*wsClient
}

// acquireTailerSubsSlice returns a reusable []*wsClient with len==0 and
// cap >= hint plus the handle the caller must hand back to release. The
// caller appends as if it were a fresh slice; only releaseTailerSubsSlice
// may return it to the pool.
func acquireTailerSubsSlice(hint int) ([]*wsClient, tailerSubsHandle) {
	sp := tailerSubsPool.Get().(*[]*wsClient)
	s := (*sp)[:0]
	if cap(s) < hint {
		s = make([]*wsClient, 0, hint)
	}
	*sp = s
	return s, tailerSubsHandle{sp: sp}
}

// releaseTailerSubsSlice clears the slice's backing pointers (so dropped
// wsClients become GC-eligible immediately) and returns it to the pool
// via the handle the caller received from acquireTailerSubsSlice. The
// final s value supersedes whatever sat in the pool (caller may have
// grown the slice via append). Nil-handle-safe so the caller can defer
// the release unconditionally even on the no-subs branch.
func releaseTailerSubsSlice(s []*wsClient, h tailerSubsHandle) {
	if h.sp == nil {
		return
	}
	for i := range s {
		s[i] = nil
	}
	*h.sp = s[:0]
	tailerSubsPool.Put(h.sp)
}

// tailerBufferedPool reuses []cli.EventEntry buffers used by attach()
// to copy the in-memory ring under lock and replay events to a new
// subscriber outside the lock. Without the pool, every agent_subscribe
// path allocated a fresh buffer of up to 500 EventEntry values
// (~140 KB) inside t.mu — the lock window is short, but the allocation
// itself is GC-visible and was the #1 attach-path alloc per
// R249-PERF-4 (#926) profiling. Reuse pattern matches tailerSubsPool:
// pointer-to-slice in the pool so the slice metadata round-trips on
// the same heap object, and a handle wrapper so the same pointer comes
// back through Put.
//
// Default cap is 16 (the typical attach replays a handful of events
// for a fresh tab joining mid-run); larger replays grow the slice via
// append() inside attach() and the grown slice returns to the pool so
// subsequent attaches at similar sizes skip the growth.
var tailerBufferedPool = sync.Pool{
	New: func() any {
		s := make([]cli.EventEntry, 0, 16)
		return &s
	},
}

// tailerBufferedHandle wraps the pool entry pointer so attach() can
// hand back the *exact* pointer it pulled. Same rationale as
// tailerSubsHandle (taking &local would force the local to escape to
// the heap on every attach call).
type tailerBufferedHandle struct {
	sp *[]cli.EventEntry
}

// acquireTailerBufferedSlice returns a reusable []cli.EventEntry with
// len==0 and cap >= hint plus the handle the caller must hand back.
// R249-PERF-4 (#926).
func acquireTailerBufferedSlice(hint int) ([]cli.EventEntry, tailerBufferedHandle) {
	sp := tailerBufferedPool.Get().(*[]cli.EventEntry)
	s := (*sp)[:0]
	if cap(s) < hint {
		s = make([]cli.EventEntry, 0, hint)
	}
	*sp = s
	return s, tailerBufferedHandle{sp: sp}
}

// releaseTailerBufferedSlice zero-clears each EventEntry (so its
// embedded pointers — Images, ToolCall, Message bytes — become
// GC-eligible immediately rather than pinning whatever the previous
// attach handed us) and returns the slice to the pool. Nil-handle-safe.
func releaseTailerBufferedSlice(s []cli.EventEntry, h tailerBufferedHandle) {
	if h.sp == nil {
		return
	}
	for i := range s {
		s[i] = cli.EventEntry{}
	}
	*h.sp = s[:0]
	tailerBufferedPool.Put(h.sp)
}
