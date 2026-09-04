//go:build !linux && !darwin

package shim

import "net"

// VerifyPeerUID is a no-op on platforms without credential passing (only
// Linux SO_PEERCRED and Darwin LOCAL_PEERCRED are implemented).
func VerifyPeerUID(_ net.Conn) bool {
	return true
}
