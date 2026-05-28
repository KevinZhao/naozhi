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

// scopeNameRe is the character-set assertion for the scopeName operand
// passed to buildBusctlArgs. systemd unit names accept the lower 7-bit
// ASCII set [a-zA-Z0-9:-_.@] and we narrow further to the literal shape
// `naozhi-shim-<int>.scope` we actually emit. Any drift trips the
// assertion below — protects future call paths that might funnel
// attacker-derived scope names into the busctl argv (R236-SEC-11 /
// R239-SEC-7). Today the sole producer is fmt.Sprintf with %d on
// cmd.Process.Pid, so the regex never rejects a legitimate value.
var scopeNameRe = regexp.MustCompile(`^naozhi-shim-[0-9]+\.scope$`)

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
//
// scopeName is asserted against scopeNameRe (R236-SEC-11 / R239-SEC-7) —
// today the producer is always fmt.Sprintf("naozhi-shim-%d.scope", PID)
// where PID is exec.Cmd.Process.Pid (validated int), so the assertion
// is pure defense-in-depth for future call paths. A mismatch returns
// nil so moveToShimsCgroup degrades to moveToShimsCgroupDirect rather
// than handing a malformed argv to sudo + busctl.
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

// readPPidFromProcStatus reads /proc/<pid>/status and returns the PPid
// field. Returns (0, err) when the file is unreadable or malformed so
// callers can decide between skipping or rejecting the operation.
//
// The status file format is one "Key:\tValue" pair per line; PPid is
// always present and a small decimal integer.
func readPPidFromProcStatus(pid int) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	// Scan rather than splitting the whole buffer:  /proc/<pid>/status
	// is short but has ~50 lines, and we only need the one starting
	// with "PPid:" — early-return saves an O(n) []string allocation.
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

// errCLIExeMismatch is wrapped into the error returned by verifyCLIExeMatch
// when /proc/<cliPID>/exe was readable but its target differs from
// wantCLIPath. Callers (today only moveToShimsCgroup) can errors.Is on
// this sentinel to distinguish the "actively-wrong binary" rejection
// (R216-SEC-5 / #546) from the readlink-failed skip path so future log
// pipelines can route the two differently without re-deriving the
// distinction from cleanExe == "".
var errCLIExeMismatch = errors.New("shim: CLI exe mismatch")

// verifyCLIExeMatch resolves /proc/<cliPID>/exe and reports whether the
// readlink target matches wantCLIPath. Returns:
//
//   - ("", err)        if /proc/<cliPID>/exe cannot be read (process gone,
//     /proc unmounted, EPERM). Caller should skip CLI adoption.
//   - (cleanExe, err)  if readlink succeeded but the target differs from
//     wantCLIPath. err wraps errCLIExeMismatch so callers can use
//     errors.Is. cleanExe is the resolved path with the
//     " (deleted)" suffix stripped (Linux appends this when
//     the binary on disk has been replaced/removed since
//     exec). Caller should refuse adoption — the shim may
//     have spawned an unintended child.
//   - (cleanExe, nil)  on match. Caller may safely adopt cliPID into the
//     privileged cgroup.
//
// Carved out of moveToShimsCgroup (R216-SEC-5 / #546) so the exe-mismatch
// branch is reachable from a unit test without driving busctl/sudo.
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
// scope so they survive service restarts. Uses busctl to call StartTransientUnit
// directly with KillMode=none, making the processes invisible to the
// naozhi.service lifecycle. Falls back to direct cgroup move if
// busctl is not available.
//
// R229-SEC-4 / R219-SEC-5: cliPID is taken from the shim's self-reported
// Hello.CLIPID frame. A compromised or buggy shim could put any PID
// (sshd, pid 1) on the wire and trick naozhi into adopting an arbitrary
// process via the privileged sudo busctl call. Validate that
// /proc/<cliPID>/status reports PPid == shimPID before passing the value
// through; on mismatch drop the CLI PID from the scope (the shim PID
// alone is still adopted via its own cmd.Process.Pid which was not
// attacker-supplied). R219-SEC-5 is the original anchor that asked for
// PPid validation; R229-SEC-4 is the implementation lane.
//
// R216-SEC-5 (#546): PPid validation defeats a forged-PID hello, but the
// shim could still spawn a child that genuinely passes PPid==shimPID yet
// is not the CLI binary (e.g. an attacker-influenced helper). Read
// /proc/<cliPID>/exe and verify it matches the configured cliPath before
// adopting; on mismatch drop the CLI PID from the scope. wantCLIPath==""
// disables the exe gate (test harnesses + Darwin-only callers that don't
// know the path); production wires StartShimWithBackend's resolved path.
func moveToShimsCgroup(parentCtx context.Context, shimPID, cliPID int, wantCLIPath string) {
	// PF1 (#399): the cgroup move is best-effort — every failure mode below
	// logs and continues, never returning to the caller. A panic here used
	// to escape into StartShim and propagate out before the caller's
	// `m.shims[key] = handle` insertion ran, leaving a live shim that the
	// manager could no longer track or shut down (orphan). All concrete
	// failures the body can encounter (busctl exit, /proc readlink, sudo
	// missing) are already non-panicking, but the recover is a structural
	// guarantee: any future regression — nil-deref on a refactored helper,
	// out-of-memory inside fmt.Sprintf, etc. — degrades to "log + continue"
	// rather than "leaked shim". Mirrors the recover wrappers already used
	// on other best-effort goroutines in this package.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("moveToShimsCgroup: recovered from panic; shim/CLI not adopted into cgroup",
				"shim_pid", shimPID, "cli_pid", cliPID, "panic", r)
		}
	}()
	scopeName := fmt.Sprintf("naozhi-shim-%d.scope", shimPID)

	// Build PID list for the scope
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
			// R216-SEC-5 (#546): belt-and-suspenders exe check. Skip the gate
			// when wantCLIPath is empty (caller doesn't know — fall back to
			// PPid alone). On Readlink error or mismatch, log + skip the CLI
			// adoption (the shim PID is still adopted via the unmodified
			// leading entry). The verification primitive is split out into
			// verifyCLIExeMatch so a unit test can exercise both branches
			// without driving the whole busctl path.
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
		// scopeName failed assertion (R236-SEC-11 / R239-SEC-7). Fall back
		// to direct cgroup move using the same already-validated PID set.
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
		// Sanitize + truncate busctl's combined stdout+stderr: D-Bus
		// diagnostics can carry bidi / C1 control bytes that would
		// otherwise corrupt journalctl rendering. 512 bytes matches the
		// existing truncation budget and aligns with R183-SEC-H1 /
		// R190-SEC-M3 precedent elsewhere in this codebase.
		sanitized := osutil.SanitizeForLog(string(out), 512)
		slog.Warn("moveToShimsCgroup: systemd scope failed, trying direct cgroup — zero-downtime restart may not survive service restart",
			"pid", shimPID, "err", err, "output", sanitized)
		// Only adopt the PIDs that passed PPid validation above (R229-SEC-4).
		// pids[0] is always the shim PID; pids[1:] (if present) is the
		// validated CLI PID.
		for _, pid := range pids {
			moveToShimsCgroupDirect(parentCtx, pid)
		}
		return
	}
	slog.Info("moved shim to independent systemd scope", "scope", scopeName, "pids", pids)
}

// moveToShimsCgroupDirect is the fallback: move a process to a root-level
// cgroup directly. Less reliable than systemd scope (systemd may still
// clean it up during restart).
//
// Caller contract (R229-SEC-4 / R230-SEC-fallback): pid MUST already be
// PPid-validated by readPPidFromProcStatus. moveToShimsCgroup is the only
// caller and only ever passes elements from its already-filtered `pids`
// slice; do not invoke this from new code paths without re-asserting that
// constraint, otherwise an attacker-controlled CLIPID could be moved into
// the privileged cgroup via the fallback `sudo tee`.
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
