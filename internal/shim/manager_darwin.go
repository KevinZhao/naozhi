//go:build darwin

package shim

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// moveToShimsCgroup is a no-op on Darwin. Linux needs an explicit cgroup /
// systemd-scope move so SIGTERM at service-stop time does not propagate to
// the shim subtree (default `KillMode=control-group` reaches every PID in
// the cgroup). launchd's default kill semantics on macOS only target the
// plist's main process, and a child started with Setsid: true is reparented
// to launchd (PID 1) when the parent exits — so the shim survives a naozhi
// restart for free, no external lifecycle move required.
func moveToShimsCgroup(_ context.Context, _, _ int, _ string) {}

// shimPIDBinaryMismatch reports whether the running process at pid is NOT
// the same binary as wantBin. Linux compares /proc/PID/exe against the
// absolute path; Darwin has no /proc, so we fall back to comparing the
// program name reported by `ps -o comm=` against filepath.Base(wantBin).
//
// This is a weaker check than the Linux path:
//   - basename collision is theoretically possible (two `naozhi` binaries
//     installed on the same host),
//   - rebuild detection is implicit (mach-O does not surface a
//     "(deleted)" marker), so the gate skips the false-positive class
//     entirely instead of stripping a suffix.
//
// In practice the gate exists to defeat PID reuse (naozhi shim exits, an
// unrelated process gets the same PID before reconnect runs); a basename
// match is sufficient for that, and matches the behavior of every other
// shim-identity check on macOS deployments. Returns (false, err) when ps
// fails so the caller can skip the gate the same way the Linux side
// handles a stat error.
func shimPIDBinaryMismatch(pid int, wantBin string) (bool, error) {
	out, err := exec.Command("/bin/ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false, fmt.Errorf("ps comm for pid %d: %w", pid, err)
	}
	got := strings.TrimSpace(string(out))
	if got == "" {
		return false, fmt.Errorf("ps returned empty comm for pid %d", pid)
	}
	return filepath.Base(got) != filepath.Base(wantBin), nil
}
