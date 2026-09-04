//go:build windows

package server

// verifyProcOwnedByEuid is unimplementable on Windows (SIDs, not POSIX
// UIDs); the stub mirrors the darwin fall-through so callers get the same
// "skip the euid check, defer to start_time" semantics.
func verifyProcOwnedByEuid(_ int) error {
	return nil
}
