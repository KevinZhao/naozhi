//go:build darwin

package shim

import (
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// VerifyPeerUID verifies the connecting peer has the same UID via the
// Darwin LOCAL_PEERCRED socket option (BSD getpeereid(3) family).
//
// Linux uses SO_PEERCRED on SOL_SOCKET (returns syscall.Ucred); macOS
// instead exposes the credential triple through SOL_LOCAL/LOCAL_PEERCRED
// and the Xucred struct. The semantics match: the kernel records the
// connecting peer's effective UID at connect() time, so a stale handle
// cannot be reused by another process.
//
// Returns false on any error (non-Unix conn, raw-conn syscall failure,
// getsockopt failure, mismatched UID). Falsy is the safe default — the
// caller closes the connection on every false return.
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
