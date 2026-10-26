//go:build linux

package shim

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// cgroupProcsPath is the fallback cgroup file written via `sudo tee` when
// busctl is unavailable; the sudoers contract test pins the exact string
// against deploy/naozhi-sudoers.example.
const cgroupProcsPath = "/sys/fs/cgroup/naozhi-shims/cgroup.procs"

// buildBusctlArgs constructs the argv tail passed to `sudo` for the
// StartTransientUnit D-Bus call that adopts shim/CLI PIDs into an independent
// systemd scope. Split out so the exact argv shape can be pinned by a unit
// test — deploy/naozhi-sudoers.example depends on these literals. The slice
// starts with "-n busctl"; moveToShimsCgroup prepends "sudo". pids is len 1
// (shim) or 2 (shim + cli); other lengths are not covered by the shipped
// sudoers policy.
func buildBusctlArgs(scopeName string, pids []int) []string {
	args := []string{"-n", "busctl", "call",
		"org.freedesktop.systemd1",
		"/org/freedesktop/systemd1",
		"org.freedesktop.systemd1.Manager",
		"StartTransientUnit",
		"ssa(sv)a(sa(sv))",
		scopeName, "fail", "2",
		"PIDs", "au", strconv.Itoa(len(pids)),
	}
	for _, p := range pids {
		args = append(args, strconv.Itoa(p))
	}
	args = append(args, "KillMode", "s", "none", "0")
	return args
}

// moveToShimsCgroup moves shim and CLI processes to an independent systemd
// scope (busctl StartTransientUnit, KillMode=none) so they survive service
// restarts, falling back to a direct cgroup move if busctl is unavailable.
func moveToShimsCgroup(parentCtx context.Context, shimPID, cliPID int) {
	// Best-effort: every failure logs and continues. The recover is a structural
	// guarantee — a panic escaping into StartShim before `m.shims[key] = handle`
	// would leave a live shim the manager can no longer track (#399).
	defer func() {
		if r := recover(); r != nil {
			slog.Error("moveToShimsCgroup: recovered from panic; shim/CLI not adopted into cgroup",
				"shim_pid", shimPID, "cli_pid", cliPID, "panic", r)
		}
	}()
	scopeName := fmt.Sprintf("naozhi-shim-%d.scope", shimPID)

	pids := []int{shimPID}
	if cliPID > 0 {
		pids = append(pids, cliPID)
	}
	args := buildBusctlArgs(scopeName, pids)

	ctx, cancel := context.WithTimeout(parentCtx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sudo", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Sanitize + truncate: D-Bus diagnostics can carry bidi / C1 control
		// bytes that corrupt journalctl rendering.
		sanitized := osutil.SanitizeForLog(string(out), 512)
		slog.Warn("moveToShimsCgroup: systemd scope failed, trying direct cgroup — zero-downtime restart may not survive service restart",
			"pid", shimPID, "err", err, "output", sanitized)
		for _, pid := range pids {
			moveToShimsCgroupDirect(parentCtx, pid)
		}
		return
	}
	slog.Info("moved shim to independent systemd scope", "scope", scopeName, "pids", pids)
}

// moveToShimsCgroupDirect is the fallback: move a process into the root-level
// naozhi-shims cgroup directly (less reliable than a systemd scope).
func moveToShimsCgroupDirect(parentCtx context.Context, pid int) {
	ctx, cancel := context.WithTimeout(parentCtx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sudo", "-n", "tee", cgroupProcsPath)
	cmd.Stdin = strings.NewReader(strconv.Itoa(pid) + "\n")
	cmd.Stdout = nil // tee copies to stdout; inherit parent (journal) is fine
	if err := cmd.Run(); err != nil {
		slog.Warn("moveToShimsCgroupDirect: failed — shim may not survive service restart", "pid", pid, "err", err)
		return
	}
	slog.Info("moved shim to independent cgroup (direct)", "pid", pid)
}

// shimPIDBinaryMismatch returns (true, nil) when /proc/PID/exe points at a
// binary other than wantBin, (false, nil) on match, and (false, err) when
// readlink failed (caller decides whether to skip the gate). The " (deleted)"
// suffix a rebuilt binary acquires is stripped so shims from the previous
// build still match.
func shimPIDBinaryMismatch(pid int, wantBin string) (bool, error) {
	exePath, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return false, err
	}
	cleanPath := strings.TrimSuffix(exePath, " (deleted)")
	return cleanPath != wantBin, nil
}
