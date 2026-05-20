// errors_usermsg.go maps internal sentinel errors from session/cli/shim into
// user-facing messages suitable for WebSocket `send_ack.error` payloads.
//
// Thin wrapper around internal/usermsg.ForSendError. The dashboard send
// path does not know the session key kind at this call site, so the
// generic phrasing for ErrNoActiveProcess applies. R226-CR-9 collapsed
// the previous duplicated switch statement here and in
// dispatch/dispatch.go onto the shared helper.
package server

import (
	"github.com/naozhi/naozhi/internal/usermsg"
)

// asyncErrorMessage returns a short Chinese user-facing label for err. It
// intentionally drops wrapping details (paths, keys, goroutine IDs) so that
// callers can pass the result straight to a browser. Unknown errors collapse
// to a generic retry hint; operators should still see the raw error in logs.
func asyncErrorMessage(err error) string {
	return usermsg.ForSendError(err, "")
}
