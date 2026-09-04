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

// moveToShimsCgroup is a no-op on Darwin: launchd's kill semantics only target
// the plist's main process, and a Setsid child is reparented to launchd on
// parent exit, so the shim survives a naozhi restart without a cgroup move.
func moveToShimsCgroup(_ context.Context, _, _ int, _ string) {}

// shimPIDBinaryMismatch reports whether the process at pid is NOT wantBin.
// Darwin has no /proc, so compare `ps -o comm=` against filepath.Base(wantBin).
// Weaker than Linux (basename collision possible, no "(deleted)" rebuild
// marker) but sufficient for its purpose: defeating PID reuse before
// reconnect. Returns (false, err) when ps fails so the caller skips the gate.
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
