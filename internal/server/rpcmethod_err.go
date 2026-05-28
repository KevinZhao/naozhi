// Phase 3e: duplicated from dashboard_session.go (one-line helper) so the
// 2 server-package call sites (dashboard_agent_events.go + wshub_send.go)
// compile after SessionHandlers moved to internal/dashboard/session.
package server

import "strings"

func isUnknownRPCMethodErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unknown method")
}
