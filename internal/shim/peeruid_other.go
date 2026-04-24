//go:build !linux

package shim

import "net"

// VerifyPeerUID is a no-op on non-Linux platforms.
// macOS does not support SO_PEERCRED; accept all local unix connections.
func VerifyPeerUID(_ net.Conn) bool {
	return true
}
