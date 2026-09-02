// install.go — the operator-triggered apply path (dashboard "一键生效").
//
// The background Checker decides WHEN to act from a timer; this file lets an
// operator decide instead, from the dashboard, without SSH. It deliberately
// adds no download or verification logic of its own: the same doInstall
// pipeline runs, so the SHA-256 check, the atomic replace, the backup and the
// rollback-on-failure behaviour are shared rather than reimplemented.
//
// What IS specific to this path is which of two things "apply" means. Under the
// default update mode the binary is usually already on disk and only a restart
// is missing, and installing again in that state overwrites the backup with the
// version we would need to roll back TO. So the whole file is organised around
// finding out whether bytes still need fetching, and refusing to fetch them
// when they do not. See docs/rfc/dashboard-update-notice.md §1.3.
package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

var (
	// ErrInstallInProgress means another install (background tick or a
	// previous click) holds installMu. Callers should surface "already
	// running" rather than queue: the operator is watching a status chip, and
	// a queued second install would either be a no-op or the very
	// backup-destroying repeat this package guards against.
	ErrInstallInProgress = errors.New("update install already in progress")

	// ErrNothingToDo means the deployment is already on the newest release, or
	// the tag we would install is the one already installed.
	ErrNothingToDo = errors.New("no update to apply")

	// ErrRestartUnsupported means a restart is the only remaining step but no
	// managed service (systemd unit / launchd job) was found to drive it. The
	// binary is on disk and correct; a human has to restart the process.
	ErrRestartUnsupported = errors.New("no managed service to restart; restart the process manually")
)

// onDiskVersionFn is indirected so tests can simulate what the binary at the
// install path reports without executing anything.
var onDiskVersionFn = onDiskVersion

// InstallLatest applies the newest release on operator demand and returns the
// outcome. `restart` asks for the service to be restarted so the new binary
// actually takes effect.
//
// Single-flight via TryLock, never a queue: a caller that arrives during an
// install gets ErrInstallInProgress immediately (the handler maps that to 409).
//
// The three ways this returns without downloading anything are the point of the
// function, not edge cases — each one is a state in which fetching would
// overwrite the rollback backup with the new binary:
//
//  1. this process already installed a newer tag (`installed`),
//  2. the newest release equals the tag already installed,
//  3. the binary sitting at the install path already reports the target
//     version — which happens when someone ran `naozhi upgrade` from a shell,
//     so `installed` is empty even though the bytes are staged.
//
// In all three the only remaining step is a restart.
func (c *Checker) InstallLatest(ctx context.Context, restart bool) error {
	if c == nil {
		return errors.New("no update checker configured")
	}
	// Same rule checkOnce and CheckNow apply: a dev build has no released tag
	// to compare against and replacing it would silently discard a local build.
	if c.cfg.CurrentVersion == "dev" || c.cfg.CurrentVersion == "" {
		return ErrCheckSkippedDev
	}

	if !c.installMu.TryLock() {
		return ErrInstallInProgress
	}
	defer c.installMu.Unlock()

	// (1) This process already staged something newer than what it runs.
	// Checked before the network call: it needs no remote information, and
	// asking GitHub first would only widen the window in which a fresher tag
	// appears and talks us into a second Replace.
	if c.installed != "" && c.installed != c.cfg.CurrentVersion {
		return c.restartOnly(ctx, c.installed)
	}

	rel, err := latestRelease(ctx)
	c.cfg.Status.noteCheck(relTag(rel), err)
	if err != nil {
		// A lookup failure is not an install failure: nothing was touched, so
		// the phase stays as it was and only CheckErr (set by noteCheck) moves.
		slog.Warn("dashboard update: release lookup failed", "err", err)
		return err
	}
	// R20260602141221-SEC-1 applies here exactly as on the tick path: only a
	// strictly greater tag is installable, so a rolled-back "latest" cannot be
	// used to push this deployment onto an older, vulnerable release.
	if !semverGreater(rel.Tag, c.cfg.CurrentVersion) {
		return ErrNothingToDo
	}
	// (2) Same tag we already installed.
	if rel.Tag == c.installed {
		return c.restartOnly(ctx, rel.Tag)
	}
	// (3) Staged by someone else — most plausibly `naozhi upgrade` run in a
	// shell, or a previous process of this service. `installed` only remembers
	// what THIS process did, so without this probe the dashboard would offer
	// "install" for a version that is already on disk and the apply would
	// Replace() a second time, copying v_new over the .bak that holds v_old.
	if onDisk, ok := onDiskVersionFn(); ok && onDisk == rel.Tag {
		slog.Info("dashboard update: target already on disk, restart only",
			"tag", rel.Tag, "running", c.cfg.CurrentVersion)
		c.installed = rel.Tag
		c.cfg.Status.notePhase(PhaseStaged, rel.Tag, nil)
		return c.restartOnly(ctx, rel.Tag)
	}

	// A real install. Downgrade the restart request rather than pretend:
	// doInstall's restart branch would enter PhaseRestarting and then call a
	// primitive that is a no-op with no managed service, leaving the chip
	// spinning on "restarting" for a process nobody is going to restart.
	wantRestart := restart
	if restart && !ServiceRunning() {
		restart = false
	}
	if err := c.doInstall(ctx, rel, restart); err != nil {
		return err
	}
	if wantRestart && !restart {
		// Installed successfully; only the restart could not happen. Report it
		// as Staged + LastErr, which is literally true and is the state the UI
		// renders with a manual command instead of a button.
		c.cfg.Status.notePhase(PhaseStaged, rel.Tag, ErrRestartUnsupported)
		return ErrRestartUnsupported
	}
	return nil
}

// restartOnly restarts the service without touching the binary. Callers must
// hold installMu.
//
// The ServiceRunning() gate here is NOT the outer gate R20260602141221-CR-3
// forbids in doInstall. That one was harmful because a stale "not running" read
// skipped the restart while `installed` had already been set, so the next tick
// short-circuited and the staged binary was stranded with no log and no notice.
// Here nothing is installed as a side effect and the verdict is returned to an
// HTTP handler that shows it to the operator, plus recorded in LastErr — the
// failure mode CR-3 protects against (silence) cannot occur.
func (c *Checker) restartOnly(ctx context.Context, tag string) error {
	if !ServiceRunning() {
		c.cfg.Status.notePhase(PhaseStaged, tag, ErrRestartUnsupported)
		return ErrRestartUnsupported
	}
	slog.Info("dashboard update: restarting to apply staged binary", "tag", tag)
	c.cfg.Status.notePhase(PhaseRestarting, tag, nil)
	c.notify("🔄 naozhi 正在重启以应用 " + tag + "…")
	if err := RestartServiceNoWait(ctx); err != nil {
		// The binary stays staged and valid; only the trigger failed. Do not
		// report Failed — that would invite a retry of an install that already
		// succeeded, which in this state is the backup-destroying path.
		slog.Warn("dashboard update: restart trigger failed (binary IS staged)", "tag", tag, "err", err)
		c.cfg.Status.notePhase(PhaseStaged, tag, err)
		return err
	}
	// Restart is queued; this process is about to receive SIGTERM.
	return nil
}

// RollbackHint returns a paste-ready shell command that restores the previous
// binary from the backup Replace() leaves behind, or "" when the install path
// cannot be resolved.
//
// It exists to be shown BEFORE the operator confirms, not after: the failure it
// answers for is "the new binary does not start", and in that failure the
// dashboard that would have told them how to recover is precisely what is gone.
// So the escape route has to be on screen while the service is still up.
//
// `serviceRunning` is supplied by the caller for the same reason as in
// CheckPreflight: it is a `launchctl list` fork on darwin, the caller already
// holds the answer, and this function is on a polled path.
func RollbackHint(serviceRunning bool) string {
	path, err := SelfPath()
	if err != nil {
		return ""
	}
	restore := fmt.Sprintf("cp %s%s %s && chmod 0755 %s", path, backupSuffix, path, path)
	// No managed service ⇒ no restart command to append. Guessing one would be
	// worse than omitting it: launchdServiceLabel() falls back to a constant
	// when this process was not launched by launchd, so the "helpful" tail would
	// be a command that fails, inside a && chain that swallows the restore's
	// success message. Restoring the bytes is the part only this hint knows.
	if !serviceRunning {
		return restore
	}
	restart := "sudo systemctl restart naozhi"
	if runtime.GOOS == "darwin" {
		restart = fmt.Sprintf("launchctl kickstart -k gui/%d/%s", os.Getuid(), launchdServiceLabel())
	}
	return restore + " && " + restart
}

// onDiskVersionProbeTimeout bounds the version probe below. Generous enough for
// a cold-cache exec of a ~19MB binary, short enough that a hung probe cannot
// hold installMu for long.
const onDiskVersionProbeTimeout = 10 * time.Second

// onDiskVersion reports what the binary at the install path claims to be, by
// running `<path> --version` (which prints the ldflag version and exits without
// reading config, opening ports, or touching state).
//
// Executing the install path is not a new capability or a new attack surface:
// it is this service's own binary, and whoever can write it already gets code
// execution at the next restart — which is exactly the event we are here to
// trigger. Doing it is what closes the gap between "what this process
// installed" and "what is actually on disk", the gap through which a `naozhi
// upgrade` from a shell would otherwise lead the dashboard into a second
// Replace().
//
// Failure is not an error: (…, false) simply means "cannot tell", and callers
// then proceed down the normal install path. The probe is a safety net, never a
// gate.
func onDiskVersion() (string, bool) {
	path, err := SelfPath()
	if err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), onDiskVersionProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		slog.Debug("dashboard update: on-disk version probe failed", "path", path, "err", err)
		return "", false
	}
	v := strings.TrimSpace(string(out))
	if i := strings.IndexByte(v, '\n'); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	// A tag is short. Anything long is not a version string — some other
	// binary, or a build that prints a banner — and comparing it to a tag
	// would be meaningless.
	if v == "" || len(v) > 64 {
		return "", false
	}
	return v, true
}
