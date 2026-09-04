// agent tailer 的两套 sync.Pool：tailerSubsPool（pollOnce fanout 路径）+
// tailerBufferedPool（attach replay 路径）。
package server

import (
	"sync"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// tailerSubsPool reuses []*wsClient slices across pollOnce ticks so the
// event-fanout path stays alloc-free in steady state (#865). Pools
// *[]*wsClient (pointer-to-slice) so the slice header does not allocate a
// fresh interface box on every Get/Put. Default cap 4 matches the typical
// 1-2 dashboard tabs; grown slices return to the pool.
var tailerSubsPool = sync.Pool{
	New: func() any {
		s := make([]*wsClient, 0, 4)
		return &s
	},
}

// tailerSubsHandle wraps the pool entry pointer so callers return the exact
// pointer they pulled from Get(); Put(&local) would force `local` to escape
// (one alloc per tick), defeating the pool.
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

// releaseTailerSubsSlice zero-clears the slice (so dropped wsClients are
// GC-eligible immediately) and returns it to the pool; the possibly-grown s
// supersedes the pooled value. Nil-handle-safe so callers can defer it.
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

// tailerBufferedPool reuses []clievent.EventEntry buffers that attach()
// fills under lock and replays outside it (#926). Same pointer-to-slice +
// handle pattern as tailerSubsPool. Default cap 16; grown slices return
// to the pool.
var tailerBufferedPool = sync.Pool{
	New: func() any {
		s := make([]clievent.EventEntry, 0, 16)
		return &s
	},
}

// tailerBufferedHandle wraps the pool entry pointer so attach() hands back
// the exact pointer it pulled; same rationale as tailerSubsHandle.
type tailerBufferedHandle struct {
	sp *[]clievent.EventEntry
}

// acquireTailerBufferedSlice returns a reusable []clievent.EventEntry with
// len==0 and cap >= hint plus the handle the caller must hand back.
func acquireTailerBufferedSlice(hint int) ([]clievent.EventEntry, tailerBufferedHandle) {
	sp := tailerBufferedPool.Get().(*[]clievent.EventEntry)
	s := (*sp)[:0]
	if cap(s) < hint {
		s = make([]clievent.EventEntry, 0, hint)
	}
	*sp = s
	return s, tailerBufferedHandle{sp: sp}
}

// releaseTailerBufferedSlice zero-clears each EventEntry (so embedded
// pointers become GC-eligible) and returns the slice to the pool. Nil-handle-safe.
func releaseTailerBufferedSlice(s []clievent.EventEntry, h tailerBufferedHandle) {
	if h.sp == nil {
		return
	}
	for i := range s {
		s[i] = clievent.EventEntry{}
	}
	*h.sp = s[:0]
	tailerBufferedPool.Put(h.sp)
}
