//go:build !linux && !darwin

package shim

import "net"

// VerifyPeerUID is a no-op on platforms without an implemented credential
// passing mechanism. Production naozhi only ships on Linux (SO_PEERCRED)
// and Darwin (LOCAL_PEERCRED); other GOOS targets compile so CI builds
// succeed but cannot enforce same-UID at the socket layer.
func VerifyPeerUID(_ net.Conn) bool {
	return true
}
