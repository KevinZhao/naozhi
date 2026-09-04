//go:build linux

package shim

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// scopeNameRe pins the scopeName operand of buildBusctlArgs to the literal
// `naozhi-shim-<int>.scope` shape we emit, so no future call path can funnel
// attacker-derived unit names into the busctl argv.
var scopeNameRe = regexp.MustCompile(`^naozhi-shim-[0-9]+\.scope$`)

// cgroupProcsPath is the fallback cgroup file written via `sudo tee` when
// busctl is unavailable; the sudoers contract test pins the exact string
// against deploy/naozhi-sudoers.example.
const cgroupProcsPath = "/sys/fs/cgroup/naozhi-shims/cgroup.procs"

// buildBusctlArgs constructs the argv tail passed to `sudo` for the
// StartTransientUnit D-Bus call that adopts shim/CLI PIDs into an independent
// systemd scope. Split out so the exact argv shape can be pinned by a unit
// test — deploy/naozhi-sudoers.example depends on these literals (see
// docs/ops/sudoers-hardening.md). The slice starts with "-n busctl";
// moveToShimsCgroup prepends "sudo".
//
// scopeName must match scopeNameRe; a mismatch returns nil so the caller
// degrades to moveToShimsCgroupDirect. pids is len 1 (shim) or 2 (shim + cli);
// other lengths are not covered by the shipped sudoers policy.
func buildBusctlArgs(scopeName string, pids []int) []string {
	if !scopeNameRe.MatchString(scopeName) {
		slog.Error("buildBusctlArgs: scope name fails character-set assertion, refusing to build argv",
			"scope", osutil.SanitizeForLog(scopeName, 128))
		return nil
	}
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

// readPPidFromProcStatus reads the PPid field from /proc/<pid>/status
// ("Key:\tValue" per line). Returns (0, err) when unreadable or malformed.
func readPPidFromProcStatus(pid int) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	// Scan line-by-line: only the PPid line is needed, so early return avoids
	// splitting the whole ~50-line buffer.
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "PPid:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("shim: malformed PPid line in /proc/%d/status", pid)
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0, fmt.Errorf("shim: parse PPid %q: %w", fields[1], err)
		}
		return ppid, nil
	}
	return 0, fmt.Errorf("shim: PPid not found in /proc/%d/status", pid)
}

// errCLIExeMismatch is wrapped by verifyCLIExeMatch when /proc/<cliPID>/exe
// was readable but differs from wantCLIPath, so callers can errors.Is the
// "wrong binary" rejection apart from the readlink-failed skip (#546).
var errCLIExeMismatch = errors.New("shim: CLI exe mismatch")

// verifyCLIExeMatch resolves /proc/<cliPID>/exe and compares it to wantCLIPath:
//
//   - ("", err): readlink failed (process gone, /proc unmounted, EPERM) —
//     caller should skip CLI adoption.
//   - (cleanExe, err wrapping errCLIExeMismatch): target differs — caller must
//     refuse adoption. cleanExe has the " (deleted)" suffix stripped.
//   - (cleanExe, nil): match — safe to adopt cliPID.
//
// Split out so the mismatch branch is unit-testable without busctl/sudo (#546).
func verifyCLIExeMatch(cliPID int, wantCLIPath string) (string, error) {
	exePath, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", cliPID))
	if err != nil {
		return "", err
	}
	cleanExe := strings.TrimSuffix(exePath, " (deleted)")
	if cleanExe != wantCLIPath {
		return cleanExe, fmt.Errorf("%w: got %q want %q", errCLIExeMismatch, cleanExe, wantCLIPath)
	}
	return cleanExe, nil
}

// moveToShimsCgroup moves shim and CLI processes to an independent systemd
// scope (busctl StartTransientUnit, KillMode=none) so they survive service
// restarts, falling back to a direct cgroup move if busctl is unavailable.
//
// cliPID comes from the shim's self-reported Hello.CLIPID, so a compromised
// shim could name any PID (sshd, pid 1) for the privileged sudo busctl call.
// Two gates before adopting it: /proc/<cliPID>/status must report PPid ==
// shimPID, and /proc/<cliPID>/exe must match wantCLIPath (a shim could spawn a
// non-CLI child that passes PPid) (#546). wantCLIPath=="" disables the exe
// gate (test harnesses). On any mismatch only the shim PID is adopted.
func moveToShimsCgroup(parentCtx context.Context, shimPID, cliPID int, wantCLIPath string) {
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
		ppid, err := readPPidFromProcStatus(cliPID)
		switch {
		case err != nil:
			// Process may have already exited (ESRCH) or /proc unreadable;
			// skip the CLI adoption rather than risk hitting an unrelated PID.
			slog.Warn("moveToShimsCgroup: cannot validate CLI PID PPid, skipping CLI adoption",
				"shim_pid", shimPID, "cli_pid", cliPID, "err", err)
		case ppid != shimPID:
			slog.Warn("moveToShimsCgroup: CLI PID PPid mismatch, refusing to adopt — shim may be compromised",
				"shim_pid", shimPID, "cli_pid", cliPID, "got_ppid", ppid)
		default:
			// Exe gate (#546): skipped when wantCLIPath is empty; on readlink
			// error or mismatch adopt the shim PID only.
			if wantCLIPath != "" {
				cleanExe, rerr := verifyCLIExeMatch(cliPID, wantCLIPath)
				switch {
				case errors.Is(rerr, errCLIExeMismatch):
					slog.Warn("moveToShimsCgroup: CLI PID exe mismatch, refusing to adopt — shim may be compromised",
						"shim_pid", shimPID, "cli_pid", cliPID, "got_exe", cleanExe, "want_exe", wantCLIPath)
				case rerr != nil:
					slog.Warn("moveToShimsCgroup: cannot readlink CLI PID exe, skipping CLI adoption",
						"shim_pid", shimPID, "cli_pid", cliPID, "err", rerr)
				default:
					pids = append(pids, cliPID)
				}
			} else {
				pids = append(pids, cliPID)
			}
		}
	}

	args := buildBusctlArgs(scopeName, pids)
	if args == nil {
		// scopeName failed scopeNameRe; fall back to a direct cgroup move with
		// the same validated PID set.
		slog.Warn("moveToShimsCgroup: scope name rejected by assertion, falling back to direct cgroup",
			"shim_pid", shimPID)
		for _, pid := range pids {
			moveToShimsCgroupDirect(parentCtx, pid)
		}
		return
	}

	ctx, cancel := context.WithTimeout(parentCtx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sudo", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Sanitize + truncate: D-Bus diagnostics can carry bidi / C1 control
		// bytes that corrupt journalctl rendering.
		sanitized := osutil.SanitizeForLog(string(out), 512)
		slog.Warn("moveToShimsCgroup: systemd scope failed, trying direct cgroup — zero-downtime restart may not survive service restart",
			"pid", shimPID, "err", err, "output", sanitized)
		// pids already holds only PPid/exe-validated entries.
		for _, pid := range pids {
			moveToShimsCgroupDirect(parentCtx, pid)
		}
		return
	}
	slog.Info("moved shim to independent systemd scope", "scope", scopeName, "pids", pids)
}

// moveToShimsCgroupDirect is the fallback: move a process into the root-level
// naozhi-shims cgroup directly (less reliable than a systemd scope).
//
// Caller contract: pid MUST already be PPid-validated. moveToShimsCgroup is the
// only caller and passes elements of its filtered `pids`; new callers must
// re-assert that or an attacker-controlled CLIPID reaches the privileged tee.
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
