// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     (none)
//	READS:      subscriber block (clients / authClients / authClientsSlice /
//	            subscriptions / subscriberCount / subscriberCountFast) for
//	            the per-key fan-out snapshot
//
// Phase 4b 起 rule 3b 升级到 AST 字段访问对账时，会校验本文件方法体
// 的字段访问匹配本契约；当前 Phase 0b 仅 marker 存在性。
package server

import (
	"errors"
	"sync/atomic"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/node"
)

// broadcastSendError pushes a `send_error` frame to every dashboard subscribed
// to key.
//
// Why a dedicated frame (#2418 follow-up F1): the WS send path answers
// asynchronous failures (spawn error, passthrough send failure) with a
// `send_ack status:error` on the originating connection. The HTTP send path —
// which every file-bearing send now takes, so upload and send derive the
// uploadStore owner from the same cookie — has no such back-channel: the 202
// is the last thing the client hears, and `sessionSend(..., nil)` used to drop
// the error on the floor. The dashboard then kept the optimistic bubble and
// the running banner with no hint that nothing was delivered.
//
// The frame is scoped to the key's subscribers (the tab that sent it is one of
// them; a second operator watching the same session sees the failure too, the
// same way the remote-send `system` event already fans out). errMsg must be the
// localised label from asyncErrorMessage — never the raw error, which can embed
// workspace paths or session keys.
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
// Informational outcomes are dropped here (M1): the WS callback can address the
// originating connection alone, but this one fans out to every subscriber of
// the key — if A's HTTP send is aborted by B's /urgent, B's tab would otherwise
// receive a send_error for its own key and tear down its own optimistic bubble
// and running flip. HTTP cannot single out the originator, so it stays silent
// and lets session_state settle the UI. Real failures still fan out.
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
// broadcastSendError below.
func (h *Hub) fanOutToSubscribers(key string, build func() any) {
	// R202606g-PERF-003 (#2308): zero-subscriber fast path. Remote/background
	// sessions frequently have nobody watching when a send/interrupt fails;
	// in that case both pool slices (candPtr + snapPtr) are taken and returned
	// unused. Probe the lock-free subscriberCountFast mirror first and bail
	// before any pool round trip when the key has zero subscribers, mirroring
	// marshalBroadcastAuth's zero-client short-circuit (R20260616-PERF-004).
	// The mirror is at most one writer-critical-section stale; a false "0"
	// only suppresses a best-effort failure notice that no live subscriber
	// could have received anyway, and the count is bumped under h.mu before
	// any client's subscriptions map carries the key.
	if h.subscriberCount != nil {
		if v, ok := h.subscriberCountFast.Load(key); !ok || v.(*atomic.Int32).Load() == 0 {
			return
		}
	}
	// Snapshot the session's subscribers BEFORE marshalling. Remote/background
	// sessions frequently have nobody watching when a send/interrupt fails —
	// in that case there is nothing to deliver, so paying the marshalPooled
	// reflect cost + a pooled-buffer round trip would be pure waste. The
	// subscriber scan is cheap (one map lookup per authenticated client) and
	// the common no-subscriber case now returns before any allocation.
	snapPtr := broadcastClientSnapPool.Get().(*[]*wsClient)
	snap := (*snapPtr)[:0]
	if h.authClients != nil {
		// R20260607-PERF-1 (#1902): two-phase snapshot so the per-key
		// subscriber filter no longer pins the Hub-wide h.mu across the whole
		// authClients walk. broadcastToAuthenticated (#1621) already moved its
		// membership scan onto the dedicated authMu so it stops serialising
		// behind register / unregister / markAuthenticated; this path lagged
		// because it ALSO needs c.subscriptions[key], which is h.mu-owned
		// (see wsclient.go markSubGenReleasable contract). Phase 1 snapshots
		// the (small) authenticated-client set under the cheap authMu.RLock and
		// releases it; phase 2 takes a short h.mu.RLock only to read each
		// candidate's subscription map, filtering in place. Lock ordering is
		// preserved — each phase takes ONE lock and releases it before the next,
		// never nesting authMu inside h.mu the way the writers do, so there is
		// no inverse-acquisition deadlock. A client unregistered between the two
		// phases is harmless: its wsClient is still live (GC-pinned by the
		// snapshot), reading its subscriptions map under h.mu.RLock is race-free,
		// and a non-blocking SendRaw to a closing client is already tolerated.
		candPtr := broadcastClientSnapPool.Get().(*[]*wsClient)
		cand := (*candPtr)[:0]
		// R202606g-PERF-020 (#2310): copy the slice mirror rather than range the
		// map (see snapshotAuthenticated).
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

		// R20260607-PERF-014 (#1925): chunk the phase-2 subscription filter so a
		// single h.mu.RLock no longer spans the entire candidate walk. The map is
		// h.mu-owned (see wsclient.go markSubGenReleasable contract) so we still
		// take h.mu.RLock to read c.subscriptions[key], but releasing it every
		// subFilterChunk candidates bounds the writer-starvation window from
		// O(N_auth_clients) to O(subFilterChunk): register / unregister /
		// markAuthenticated (which take the h.mu WRITE lock) can interleave
		// between chunks instead of waiting behind the whole fleet's scan. Each
		// candidate wsClient is GC-pinned by `cand`, so a client unregistered
		// between chunks is still a live, race-free read under the next RLock;
		// a non-blocking SendRaw to a closing client is already tolerated. Lock
		// ordering is unchanged — each chunk takes ONE lock and releases it, never
		// nesting authMu inside h.mu the way the writers do.
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
