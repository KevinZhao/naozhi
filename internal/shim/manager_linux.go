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

// cgroupProcsPath is the fixed fallback cgroup file naozhi writes to via
// `sudo tee` when busctl is unavailable. Exposed as a package-level const
// so the sudoers policy contract test can assert the exact string and
// deploy/naozhi-sudoers.example stays synced.
const cgroupProcsPath = "/sys/fs/cgroup/naozhi-shims/cgroup.procs"

// buildBusctlArgs constructs the argv tail passed to `sudo` for the
// StartTransientUnit D-Bus call that adopts shim/CLI PIDs into an
// independent systemd scope. Split out from moveToShimsCgroup so the
// exact argv shape can be pinned by a unit test — the
// deploy/naozhi-sudoers.example policy depends on these literals not
// drifting (see docs/ops/sudoers-hardening.md). The returned slice
// starts with the "-n" non-interactive flag and the "busctl" command
// name; moveToShimsCgroup prepends "sudo" via exec.CommandContext.
//
// scopeName must already be the final "naozhi-shim-<PID>.scope" form.
// pids is expected to be len 1 (shim only) or 2 (shim + cli). Other
// lengths are permitted but are not covered by the shipped sudoers
// policy — callers that change the expected range must update both
// this function's contract test and the Cmnd_Alias set in the policy.
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
// scope so they survive service restarts. Uses busctl to call StartTransientUnit
// directly with KillMode=none, making the processes invisible to the
// naozhi.service lifecycle. Falls back to direct cgroup move if
// busctl is not available.
func moveToShimsCgroup(parentCtx context.Context, shimPID, cliPID int) {
	scopeName := fmt.Sprintf("naozhi-shim-%d.scope", shimPID)

	// Build PID list for the scope
	pids := []int{shimPID}
	if cliPID > 0 {
		pids = append(pids, cliPID)
	}

	args := buildBusctlArgs(scopeName, pids)

	ctx, cancel := context.WithTimeout(parentCtx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sudo", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Sanitize + truncate busctl's combined stdout+stderr: D-Bus
		// diagnostics can carry bidi / C1 control bytes that would
		// otherwise corrupt journalctl rendering. 512 bytes matches the
		// existing truncation budget and aligns with R183-SEC-H1 /
		// R190-SEC-M3 precedent elsewhere in this codebase.
		sanitized := osutil.SanitizeForLog(string(out), 512)
		slog.Warn("moveToShimsCgroup: systemd scope failed, trying direct cgroup — zero-downtime restart may not survive service restart",
			"pid", shimPID, "err", err, "output", sanitized)
		moveToShimsCgroupDirect(parentCtx, shimPID)
		if cliPID > 0 {
			moveToShimsCgroupDirect(parentCtx, cliPID)
		}
		return
	}
	slog.Info("moved shim to independent systemd scope", "scope", scopeName, "pids", pids)
}

// moveToShimsCgroupDirect is the fallback: move a process to a root-level
// cgroup directly. Less reliable than systemd scope (systemd may still
// clean it up during restart).
func moveToShimsCgroupDirect(parentCtx context.Context, pid int) {
	// The procs path is pinned as a package-level const so the sudoers
	// policy contract test can diff it against the shipped
	// deploy/naozhi-sudoers.example literal; drifting one without the
	// other would silently start rejecting the fallback tee at runtime.
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
// binary other than wantBin, (false, nil) when it matches, and
// (false, err) when the readlink failed (caller decides whether to skip
// the gate). Linux marks a rebuilt binary's exe entry as "<path> (deleted)"
// — strip that suffix so a freshly recompiled naozhi still recognises
// shims spawned by the previous build.
func shimPIDBinaryMismatch(pid int, wantBin string) (bool, error) {
	exePath, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return false, err
	}
	cleanPath := strings.TrimSuffix(exePath, " (deleted)")
	return cleanPath != wantBin, nil
}
