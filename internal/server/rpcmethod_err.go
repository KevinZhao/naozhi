package server

import "strings"

// isUnknownRPCMethodErr reports whether err is the session RPC "unknown method" error.
func isUnknownRPCMethodErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unknown method")
}
