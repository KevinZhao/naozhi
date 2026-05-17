//go:build !linux && !darwin

package discovery

// ProcStartTime stub for Windows. The shim/discovery stack is POSIX-only;
// release.yml excludes windows, and CI's build-windows job is a
// compile-only regression gate (ci.yml continue-on-error). This stub
// lets callers outside internal/discovery (upstream.connector,
// server.takeover, server.dashboard_discovered) compile across
// GOOS=windows without branching on build tags at every call site.
func ProcStartTime(_ int) (uint64, error) {
	return 0, ErrUnsupportedPlatform
}

func detectCLIName(_ int) string { return "cli" }
func procPidAlive(_ int) bool    { return false }
func procKillSIGKILL(_ int)      {}
