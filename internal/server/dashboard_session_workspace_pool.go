package server

import "sync"

// workspaceSlicePool reuses the []string scratch that handleList builds to
// pass into ProjectManager.ResolveWorkspaces. The dashboard polls that path
// at 1 Hz × N tabs and the previous code allocated a fresh slice every
// time even though ResolveWorkspaces never retains it. The pool drops the
// steady-state alloc to a single sync.Pool slot lookup.
//
// R217-PERF-10 / #616.
//
// Sizing: typical dashboards see <100 sessions; we pre-size to a hint
// passed in by the caller so a single-poll spike still gets a right-sized
// underlying array. Acquired slices keep whatever capacity they grow to;
// the Put side zero-clears element pointers so workspace strings cannot
// be kept alive past their session's lifetime.
//
// The pool is exclusive to this file's hot path. Tests in
// dashboard_session_workspace_pool_test.go pin the
// reuse + zero-clear contract.
var workspaceSlicePool = sync.Pool{
	New: func() any {
		s := make([]string, 0, 64)
		return &s
	},
}

// acquireWorkspaceSlice returns a []string with len 0 and capacity at
// least hint. The returned slice MUST be released via releaseWorkspaceSlice
// after the caller is done reading it; ResolveWorkspaces is the only
// downstream consumer and does not retain its input, so a deterministic
// release on the same goroutine is sufficient.
func acquireWorkspaceSlice(hint int) []string {
	pp := workspaceSlicePool.Get().(*[]string)
	s := *pp
	if cap(s) < hint {
		// Hint exceeds the pooled capacity; allocate a right-sized slice.
		// We still return the original to the pool so it stays warm for
		// the steady-state size class.
		workspaceSlicePool.Put(pp)
		return make([]string, 0, hint)
	}
	return s[:0]
}

// releaseWorkspaceSlice zero-clears the element pointers and returns the
// backing array to the pool. Callers MUST NOT use the slice after this
// call. Passing a nil slice is a no-op.
func releaseWorkspaceSlice(s []string) {
	if s == nil {
		return
	}
	// Zero so the strings are reachable for GC immediately. Without this
	// the pool would keep the last poll's workspace strings alive until
	// the next caller overwrote them.
	for i := range s {
		s[i] = ""
	}
	s = s[:0]
	workspaceSlicePool.Put(&s)
}
