//go:build windows

package server

// verifyProcOwnedByEuid is unimplementable on Windows: NTFS uses SIDs, not
// POSIX UIDs, and naozhi's takeover path (PID/start_time TOCTOU guard for
// reusing a stranded Claude CLI) is a Linux-only feature. The Unix
// implementation already returns nil on darwin (no /proc); the Windows
// stub mirrors that fall-through so callers get the same "skip the
// euid check, defer to start_time" semantics. naozhi production runs
// Linux; Windows is a build-only CI gate.
func verifyProcOwnedByEuid(_ int) error {
	return nil
}
