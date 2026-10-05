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
func (b *wsBroadcaster) broadcastSendError(key, errMsg string) {
	if key == "" || errMsg == "" {
		return
	}
	b.fanOutToSubscribers(key, func() any {
		return wsproto.NewSendError(wsproto.SendError{Key: key, Error: errMsg})
	})
}

// informationalSendErr reports whether err is a passthrough outcome the user
// already knows about: a /clear-/new reset or a reconnect with unknown state.
// session_state corrects the UI for both.
func informationalSendErr(err error) bool {
	return errors.Is(err, clierr.ErrSessionReset) ||
		errors.Is(err, clierr.ErrReconnectedUnknown)
}

// fanOutToSubscribers delivers one frame to every authenticated client
// subscribed to key. `build` is invoked (and the frame marshalled) only when
// at least one subscriber exists, so callers on hot failure paths pay nothing
// for unwatched sessions. Shared by broadcastSessionSystemEvent and
// broadcastSendError.
func (b *wsBroadcaster) fanOutToSubscribers(key string, build func() any) {
	// Zero-subscriber fast path before any pool round trip or marshal. The
	// lock-free count is at most one critical section stale; a false "0" only
	// suppresses a best-effort notice no live subscriber could have received.
	if b.recipients.count(key) == 0 {
		return
	}
	snapPtr := broadcastClientSnapPool.Get().(*[]*wsClient)
	snap := b.recipients.subscribersOf(key, (*snapPtr)[:0])

	if len(snap) > 0 {
		if data, err := marshalPooled(build()); err == nil {
			for _, c := range snap {
				c.SendRaw(data)
			}
		}
	}

	releaseBroadcastSnap(snapPtr, snap)
}
