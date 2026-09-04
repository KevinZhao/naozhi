// errors_usermsg.go maps internal sentinel errors from session/cli/shim into
// user-facing messages suitable for WebSocket `send_ack.error` payloads.
//
// Thin wrapper around internal/usermsg.ForSendError; the send path does not
// know the session key kind here, so the generic ErrNoActiveProcess phrasing applies.
package server

import (
	"github.com/naozhi/naozhi/internal/usermsg"
)

// asyncErrorMessage returns a short Chinese user-facing label for err. It
// drops wrapping details (paths, keys, goroutine IDs) so the result can go
// straight to a browser; unknown errors collapse to a generic retry hint.
func asyncErrorMessage(err error) string {
	return usermsg.ForSendError(err, "")
}
