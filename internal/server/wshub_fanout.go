package server

import (
	"errors"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/wsproto"
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
		return wsproto.NewSendError(wsproto.SendError{Key: key, Error: errMsg})
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
	return errors.Is(err, clierr.ErrAbortedByUrgent) ||
		errors.Is(err, clierr.ErrSessionReset) ||
		errors.Is(err, clierr.ErrReconnectedUnknown)
}

// fanOutToSubscribers delivers one frame to every authenticated client
// subscribed to key. `build` is invoked (and the frame marshalled) only when
// at least one subscriber exists, so callers on hot failure paths pay nothing
// for unwatched sessions. Shared by broadcastSessionSystemEvent and
// broadcastSendError.
func (h *Hub) fanOutToSubscribers(key string, build func() any) {
	// Zero-subscriber fast path before any pool round trip or marshal. The
	// lock-free count is at most one critical section stale; a false "0" only
	// suppresses a best-effort notice no live subscriber could have received.
	if h.subs.count(key) == 0 {
		return
	}
	snapPtr := broadcastClientSnapPool.Get().(*[]*wsClient)
	snap := h.subs.subscribersOf(key, (*snapPtr)[:0])

	if len(snap) > 0 {
		if data, err := marshalPooled(build()); err == nil {
			for _, c := range snap {
				c.SendRaw(data)
			}
		}
	}

	releaseBroadcastSnap(snapPtr, snap)
}
