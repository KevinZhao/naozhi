//go:build darwin

package shim

import (
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// VerifyPeerUID verifies the connecting peer has the same UID via Darwin's
// SOL_LOCAL/LOCAL_PEERCRED (Xucred), the counterpart of Linux SO_PEERCRED:
// the kernel records the peer's effective UID at connect() time.
// Returns false on any error — the caller closes the connection.
func VerifyPeerUID(conn net.Conn) bool {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return false
	}
	var cred *unix.Xucred
	var credErr error
	if ctrlErr := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); ctrlErr != nil {
		return false
	}
	if credErr != nil || cred == nil {
		return false
	}
	return cred.Uid == uint32(os.Getuid())
}
