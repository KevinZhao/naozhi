// install.go — the operator-triggered apply path (dashboard "一键生效"). It
// reuses doInstall; what is specific here is deciding whether bytes still need
// fetching: installing again in the staged state overwrites the backup with
// the very version we would roll back TO (docs/rfc/dashboard-update-notice.md §1.3).
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
	// ErrInstallInProgress means another install holds installMu; surface
	// "already running" rather than queue a possibly backup-destroying repeat.
	ErrInstallInProgress = errors.New("update install already in progress")

	// ErrNothingToDo means the deployment is already on the newest release.
	ErrNothingToDo = errors.New("no update to apply")

	// ErrRestartUnsupported means only a restart remains but no managed service
	// can drive it; the binary is correct and a human must restart.
	ErrRestartUnsupported = errors.New("no managed service to restart; restart the process manually")
)

// onDiskVersionFn is indirected so tests can simulate the on-disk binary's version.
var onDiskVersionFn = onDiskVersion

// InstallLatest applies the newest release on operator demand; `restart` asks
// for the service restart that makes it take effect. Single-flight via
// TryLock (ErrInstallInProgress, never a queue). It returns WITHOUT
// downloading — restart only — when this process already installed a newer
// tag, when latest equals the installed tag, or when the on-disk binary
// already reports the target version (someone ran `naozhi upgrade` from a
// shell): in each state fetching would overwrite the rollback backup.
func (c *Checker) InstallLatest(ctx context.Context, restart bool) error {
	if c == nil {
		return errors.New("no update checker configured")
	}
	if c.cfg.CurrentVersion == "dev" || c.cfg.CurrentVersion == "" {
		return ErrCheckSkippedDev
	}

	if !c.installMu.TryLock() {
		return ErrInstallInProgress
	}
	defer c.installMu.Unlock()

	// (1) Already staged something newer; checked before the network call so a
	// fresher remote tag cannot talk us into a second Replace.
	if c.installed != "" && c.installed != c.cfg.CurrentVersion {
		return c.restartOnly(ctx, c.installed)
	}

	rel, err := latestRelease(ctx)
	c.cfg.Status.noteCheck(relTag(rel), err)
	if err != nil {
		// A lookup failure touches nothing; only CheckErr moves.
		slog.Warn("dashboard update: release lookup failed", "err", err)
		return err
	}
	// Strictly greater only, so a rolled-back "latest" cannot downgrade us.
	if !semverGreater(rel.Tag, c.cfg.CurrentVersion) {
		return ErrNothingToDo
	}
	// (2) Same tag we already installed.
	if rel.Tag == c.installed {
		return c.restartOnly(ctx, rel.Tag)
	}
	// (3) Staged by someone else; `installed` only remembers THIS process.
	if onDisk, ok := onDiskVersionFn(); ok && onDisk == rel.Tag {
		slog.Info("dashboard update: target already on disk, restart only",
			"tag", rel.Tag, "running", c.cfg.CurrentVersion)
		c.installed = rel.Tag
		c.cfg.Status.notePhase(PhaseStaged, rel.Tag, nil)
		return c.restartOnly(ctx, rel.Tag)
	}

	// Downgrade the restart request rather than pretend: with no service
	// managing US, PhaseRestarting would spin forever. ServiceManagesThisProcess,
	// not ServiceRunning — a unit running another naozhi would take the restart.
	wantRestart := restart
	if restart && !ServiceManagesThisProcess() {
		restart = false
	}
	if err := c.doInstall(ctx, rel, restart); err != nil {
		return err
	}
	if wantRestart && !restart {
		// Installed; only the restart could not happen. Staged + LastErr is the
		// state the UI renders with a manual command instead of a button.
		c.cfg.Status.notePhase(PhaseStaged, rel.Tag, ErrRestartUnsupported)
		return ErrRestartUnsupported
	}
	return nil
}

// restartOnly restarts the service without touching the binary; callers hold
// installMu. Unlike doInstall, gating on ServiceManagesThisProcess here is
// safe: nothing is installed as a side effect and the verdict reaches the
// operator via the HTTP handler and LastErr, so it cannot strand silently.
func (c *Checker) restartOnly(ctx context.Context, tag string) error {
	if !ServiceManagesThisProcess() {
		c.cfg.Status.notePhase(PhaseStaged, tag, ErrRestartUnsupported)
		return ErrRestartUnsupported
	}
	slog.Info("dashboard update: restarting to apply staged binary", "tag", tag)
	c.cfg.Status.notePhase(PhaseRestarting, tag, nil)
	c.notify("🔄 naozhi 正在重启以应用 " + tag + "…")
	if err := RestartServiceNoWait(ctx); err != nil {
		// Staged (with LastErr), not Failed: Failed would invite a repeat
		// install, the backup-destroying path.
		slog.Warn("dashboard update: restart trigger failed (binary IS staged)", "tag", tag, "err", err)
		c.cfg.Status.notePhase(PhaseStaged, tag, err)
		return err
	}
	// Restart queued; this process is about to receive SIGTERM.
	return nil
}

// RollbackHint returns a paste-ready command restoring the previous binary
// from Replace's backup, or "" when the install path is unknown. Shown BEFORE
// the operator confirms: if the new binary does not start, the dashboard that
// would explain recovery is gone. serviceRunning is caller-supplied (a
// launchctl fork on darwin the caller already paid for).
func RollbackHint(serviceRunning bool) string {
	path, err := SelfPath()
	if err != nil {
		return ""
	}
	restore := fmt.Sprintf("cp %s%s %s && chmod 0755 %s", path, backupSuffix, path, path)
	// No managed service ⇒ no restart tail: a guessed launchd command would
	// fail inside the && chain and hide the restore's success.
	if !serviceRunning {
		return restore
	}
	return restore + " && " + restartCommand()
}

// restartCommand is the shell command that restarts the managed service on
// this host; only meaningful when one exists (callers gate on that).
func restartCommand() string {
	if runtime.GOOS == "darwin" {
		return fmt.Sprintf("launchctl kickstart -k gui/%d/%s", os.Getuid(), launchdServiceLabel())
	}
	return "sudo systemctl restart naozhi"
}

// ManualCommand is what the dashboard shows when it cannot carry out `action`
// itself, computed server-side because the browser knows neither the server's
// platform nor its launchd label. ActionInstall → `naozhi upgrade`;
// ActionRestart → the platform restart command only when a service manages
// this process (otherwise "" — `systemctl restart` would hit the wrong thing).
func ManualCommand(action Action, serviceManaged bool) string {
	switch action {
	case ActionInstall:
		return "naozhi upgrade"
	case ActionRestart:
		if serviceManaged {
			return restartCommand()
		}
	}
	return ""
}

// onDiskVersionProbeTimeout bounds the version probe so a hung exec cannot
// hold installMu for long.
const onDiskVersionProbeTimeout = 10 * time.Second

// onDiskVersion runs `<installPath> --version` to learn what is actually on
// disk. Not a new attack surface: whoever can write our own binary already
// gets code execution at the next restart. (…, false) means "cannot tell" and
// callers proceed down the normal install path — a safety net, never a gate.
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
	// Anything long is a banner or another binary, not a tag.
	if v == "" || len(v) > 64 {
		return "", false
	}
	return v, true
}
