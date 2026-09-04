//go:build !linux && !darwin

package discovery

// ProcStartTime stub for non-POSIX targets. The shim/discovery stack is
// POSIX-only; this stub lets callers outside internal/discovery compile on
// GOOS=windows without build-tag branching at every call site.
func ProcStartTime(_ int) (uint64, error) {
	return 0, ErrUnsupportedPlatform
}

func detectCLIName(_ int) string { return "cli" }
func procPidAlive(_ int) bool    { return false }
func procKillSIGKILL(_ int)      {}
