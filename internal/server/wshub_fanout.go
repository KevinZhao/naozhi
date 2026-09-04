// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     (none)
//	READS:      subscriber block (clients / authClients / authClientsSlice /
//	            subscriptions / subscriberCount / subscriberCountFast) for
//	            the per-key fan-out snapshot
package server

import (
	"errors"
	"sync/atomic"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/node"
)

// broadcastSendError pushes a `send_error` frame to every dashboard subscribed
// to key. The HTTP send path (which every file-bearing send takes) has no
// per-connection back-channel like the WS send_ack, so its asynchronous
// failures are reported over this subscriber-scoped frame instead (#2418).
//
// errMsg must be the localised label from asyncErrorMessage — never the raw
// error, which can embed workspace paths or session keys.
func (h *Hub) broadcastSendError(key, errMsg string) {
	if key == "" || errMsg == "" {
		return
	}
	h.fanOutToSubscribers(key, func() any {
		return node.ServerMsg{Type: "send_error", Key: key, Error: errMsg}
	})
}

// asyncErrorFn is the sessionSend post-ack failure callback: err is the
// underlying error (nil at the literal-message sites — interrupt timeout,
// owner-loop panic), msg the localised user-facing label.
type asyncErrorFn func(err error, msg string)

// informationalSendErr reports whether err is a passthrough outcome the user
// already knows about: their own /urgent preemption, a /clear-/new reset, or a
// reconnect with unknown state. session_state corrects the UI for all three.
func informationalSendErr(err error) bool {
	return errors.Is(err, cli.ErrAbortedByUrgent) ||
		errors.Is(err, cli.ErrSessionReset) ||
		errors.Is(err, cli.ErrReconnectedUnknown)
}

// httpSendErrorCallback adapts broadcastSendError to the sessionSend
// onAsyncError signature for the HTTP send path.
//
// Informational outcomes are dropped: this callback fans out to every
// subscriber of the key, so if A's HTTP send is aborted by B's /urgent, B's
// tab would otherwise tear down its own optimistic bubble. session_state
// settles the UI instead; real failures still fan out.
func (h *Hub) httpSendErrorCallback(key string) asyncErrorFn {
	return func(err error, errMsg string) {
		if informationalSendErr(err) {
			return
		}
		h.broadcastSendError(key, errMsg)
	}
}

// fanOutToSubscribers delivers one frame to every authenticated client
// subscribed to key. `build` is invoked (and the frame marshalled) only when
// at least one subscriber exists, so callers on hot failure paths pay nothing
// for unwatched sessions. Shared by broadcastSessionSystemEvent and
// broadcastSendError.
func (h *Hub) fanOutToSubscribers(key string, build func() any) {
	// Zero-subscriber fast path before any pool round trip. The lock-free
	// subscriberCountFast mirror is at most one writer critical section stale;
	// a false "0" only suppresses a best-effort notice no live subscriber could
	// have received, and the count is bumped under h.mu before any client's
	// subscriptions map carries the key (#2308).
	if h.subscriberCount != nil {
		if v, ok := h.subscriberCountFast.Load(key); !ok || v.(*atomic.Int32).Load() == 0 {
			return
		}
	}
	// Snapshot subscribers BEFORE marshalling so unwatched sessions pay no
	// marshalPooled reflect cost or pooled-buffer round trip.
	snapPtr := broadcastClientSnapPool.Get().(*[]*wsClient)
	snap := (*snapPtr)[:0]
	if h.authClients != nil {
		// Two-phase snapshot: phase 1 copies the authenticated set under authMu;
		// phase 2 reads each candidate's h.mu-owned subscriptions map (see
		// wsclient.go markSubGenReleasable) under a short h.mu.RLock. Each phase
		// takes ONE lock and releases it — authMu is never nested inside h.mu
		// (the writers' order), so no inverse-acquisition deadlock (#1902).
		candPtr := broadcastClientSnapPool.Get().(*[]*wsClient)
		cand := (*candPtr)[:0]
		// Copy the slice mirror rather than range the map (see snapshotAuthenticated).
		h.authMu.RLock()
		if n := len(h.authClientsSlice); n > 0 {
			if cap(cand) < n {
				cand = make([]*wsClient, n)
			} else {
				cand = cand[:n]
			}
			copy(cand, h.authClientsSlice)
		}
		h.authMu.RUnlock()

		// Chunk the filter so one h.mu.RLock never spans the whole candidate
		// walk; register / unregister / markAuthenticated (write lock) interleave
		// between chunks (#1925). A client unregistered between chunks is still
		// GC-pinned by cand, so the read stays race-free, and a non-blocking
		// SendRaw to a closing client is tolerated.
		for start := 0; start < len(cand); start += subFilterChunk {
			end := start + subFilterChunk
			if end > len(cand) {
				end = len(cand)
			}
			h.mu.RLock()
			for _, c := range cand[start:end] {
				if _, ok := c.subscriptions[key]; ok {
					snap = append(snap, c)
				}
			}
			h.mu.RUnlock()
		}
		releaseBroadcastSnap(candPtr, cand)
	} else {
		// Legacy fallback for hand-rolled hubs that never initialise
		// authClients. The clients map is h.mu-owned, so membership and the
		// subscription filter share the single Hub-wide RLock here.
		h.mu.RLock()
		for c := range h.clients {
			if !c.authenticated.Load() {
				continue
			}
			if _, ok := c.subscriptions[key]; ok {
				snap = append(snap, c)
			}
		}
		h.mu.RUnlock()
	}

	if len(snap) > 0 {
		if data, err := marshalPooled(build()); err == nil {
			for _, c := range snap {
				c.SendRaw(data)
			}
		}
	}

	releaseBroadcastSnap(snapPtr, snap)
}
