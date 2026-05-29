// File: service_systemd.go
//
// Phase 5-prep / R-cmd-service-systemd-extract (2026-05-28):
// 把 service.go 中 Linux/systemd 安装逻辑抽到独立文件。**纯物理切分，
// 逐字保留原代码、零行为变化**。
//
// 抽出的内容（按 origin/master service.go line 144-438 原貌）：
//   - generateSystemdUnit       渲染 .service unit 文件
//   - installSystemdPlan type + steps()  安装计划（dry-run 可打印）
//   - planInstallSystemd        计算安装步骤（纯函数，可单测）
//   - systemctlIsActive var     systemctl is-active 探测（测试可替换）
//   - rewriteUnitWithRollback   原子写 unit + 失败回滚
//   - installSystemd            installSystemd 主流程
//   - recoveryHint              失败恢复提示
//   - uninstallSystemd          卸载
//
// systemd 与 launchd（macOS，留在 service.go）零交叉引用。const
// systemdUnitPath / systemdUnitBackupSuffix + helper run / fatalf 留在
// service.go，通过同 package 可见性继续使用。
package main

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"os/exec"
	"strings"
)

// --- systemd (Linux) ---

func generateSystemdUnit(binary, configPath, user, home string) string {
	// Type=notify + WatchdogSec=120: main.go uses sd_notify("READY=1")
	// once the listener is bound and periodically re-pings to keep the
	// watchdog from firing. Using Type=simple (or omitting WatchdogSec)
	// produces a tight "sd_notify READY failed" log loop on restart.
	//
	// KillMode=process + SendSIGKILL=no + TimeoutStopSec=5: shims are
	// long-lived helper processes moved into /sys/fs/cgroup/naozhi-shims/
	// so they persist across naozhi restarts for zero-downtime reconnect.
	// The default control-group kill mode would SIGKILL every shim on
	// systemctl stop/restart, losing in-flight CLI sessions. Matching
	// deploy/naozhi.service so both install paths produce the same
	// service semantics.
	return fmt.Sprintf(`[Unit]
Description=naozhi - Claude Code IM Gateway
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
NotifyAccess=main
WatchdogSec=120
ExecStart="%s" --config "%s"
WorkingDirectory=%s
Restart=always
RestartSec=5
StartLimitInterval=60s
StartLimitBurst=5
User=%s
Environment=HOME=%s
KillMode=process
SendSIGKILL=no
TimeoutStopSec=5

[Install]
WantedBy=multi-user.target
`, binary, configPath, home, user, home)
}

// installSystemdPlan classifies the actions required to converge the host
// into the desired systemd state. Separated from the effectful wrapper so
// tests can assert the decision matrix without touching the real /etc or
// shelling out to systemctl.
type installSystemdPlan struct {
	// UnitChanged is true when the rendered unit differs from the one
	// already on disk (or no unit is on disk yet). Drives whether we
	// rewrite /etc/systemd/system/naozhi.service and call daemon-reload.
	UnitChanged bool
	// ServiceActive mirrors `systemctl is-active naozhi` at plan time.
	// Decides between `start` (not active) and `restart` (already active
	// + unit changed) on the final hop.
	ServiceActive bool
}

// planInstallSystemd derives the plan from the rendered-vs-existing unit
// bytes and the current service state. Pure function — no side effects —
// so it is trivially unit-tested.
//
// `force=true` promotes UnitChanged to true regardless of byte-equality,
// so a rerun with a known-good unit still triggers daemon-reload + the
// final restart/start hop. Used by the `-force` flag to recover from a
// hand-edited unit file or push a binary swap without the unit diffing.
func planInstallSystemd(renderedUnit, existingUnit string, existingUnitErr error, isActive, force bool) installSystemdPlan {
	unitChanged := true
	if !force && existingUnitErr == nil && existingUnit == renderedUnit {
		unitChanged = false
	}
	return installSystemdPlan{
		UnitChanged:   unitChanged,
		ServiceActive: isActive,
	}
}

// systemctlIsActive reports whether `systemctl is-active <name>` exits 0.
// Separated so tests can stub it. A non-zero exit (service not running,
// not installed, or systemctl error) is treated as "not active", which
// falls back to the safe `start` branch on the caller side.
var systemctlIsActive = func(name string) bool {
	cmd := exec.Command("systemctl", "is-active", "--quiet", name)
	return cmd.Run() == nil
}

// rewriteUnitWithRollback writes `renderedUnit` atomically to `unitPath`,
// first snapshotting the current contents so a failed `daemon-reload` can
// roll back instead of leaving a broken unit on disk. Pure I/O + injected
// runners so tests can drive every branch without touching real systemctl.
//
// Branches:
//
//	existingUnit="", readErr=IsNotExist       — fresh install, no backup needed
//	existingUnit=X, readErr=nil               — snapshot to backupPath, then write
//	reload fails after rewrite                — attempt restore backup + second reload
//	reload success                            — rm backup (best-effort)
//
// Return value reports the failure that surfaced to the operator. On rollback,
// even a successful restore still returns the original reload error so the
// installer aborts the rest of the flow (enable/start) — the unit on disk
// now matches the previously-good state but systemd may be in a weird
// transient state, so re-running install is the safer recovery.
func rewriteUnitWithRollback(unitPath, renderedUnit, existingUnit string, readErr error, writeFile func(name string, data []byte, perm os.FileMode) error, removeFile func(string) error, daemonReload func() error) error {
	backupPath := unitPath + systemdUnitBackupSuffix
	hadExisting := readErr == nil
	if hadExisting {
		// Snapshot the current unit before overwriting. If the snapshot
		// write itself fails, propagate — a rollback we can't honor is
		// worse than not trying, since the operator may believe the
		// network-safety net exists when it doesn't.
		if err := writeFile(backupPath, []byte(existingUnit), 0644); err != nil {
			return fmt.Errorf("snapshot existing unit to %s: %w", backupPath, err)
		}
	}

	if err := writeFile(unitPath, []byte(renderedUnit), 0644); err != nil {
		// Best-effort cleanup: if we wrote a backup, leave it in place so
		// the operator can inspect/restore manually. The partial write
		// failure may have left the unit file in an indeterminate state.
		return fmt.Errorf("write unit file: %w", err)
	}

	reloadErr := daemonReload()
	if reloadErr == nil {
		// Success: drop the snapshot so the next install path starts
		// from a clean state. Failure here is non-fatal — a stale .bak
		// only costs a few KiB and the next install overwrites it.
		if hadExisting {
			_ = removeFile(backupPath)
		}
		return nil
	}

	// Reload failed. Try to restore the backup so the unit on disk
	// matches the previously-good state. If there was no backup (fresh
	// install), the least-bad option is to leave the freshly-written
	// unit in place and surface the reload error.
	if !hadExisting {
		return fmt.Errorf("daemon-reload: %w (no prior unit to restore)", reloadErr)
	}
	if restoreErr := writeFile(unitPath, []byte(existingUnit), 0644); restoreErr != nil {
		return fmt.Errorf("daemon-reload: %w (rollback ALSO failed: %v; inspect %s and %s manually)",
			reloadErr, restoreErr, unitPath, backupPath)
	}
	// Try one more reload so systemd's in-memory view catches up with
	// the restored bytes. If this also fails, the on-disk state is at
	// least known-good; the operator needs to kick systemd manually.
	if secondReloadErr := daemonReload(); secondReloadErr != nil {
		return fmt.Errorf("daemon-reload: %w (unit rolled back to prior contents but second reload failed: %v; try `sudo systemctl daemon-reload` manually)",
			reloadErr, secondReloadErr)
	}
	// Backup served its purpose — drop it. Return the original reload
	// error so the outer installer aborts enable/start; re-running
	// `naozhi install` is the clean recovery path.
	_ = removeFile(backupPath)
	return fmt.Errorf("daemon-reload: %w (unit rolled back to prior contents; re-run `sudo naozhi install` after fixing the underlying issue)", reloadErr)
}

func installSystemd(binary, configPath string, dryRun, force bool) {
	if !dryRun && os.Getuid() != 0 {
		fatalf("systemd install requires root. Run: sudo naozhi install")
	}

	user, home := serviceUser()
	unit := generateSystemdUnit(binary, configPath, user, home)

	existingBytes, existingErr := os.ReadFile(systemdUnitPath)
	plan := planInstallSystemd(unit, string(existingBytes), existingErr, systemctlIsActive("naozhi"), force)

	if dryRun {
		fmt.Printf("unit path:       %s\n", systemdUnitPath)
		fmt.Printf("unit changed:    %t\n", plan.UnitChanged)
		fmt.Printf("service active:  %t\n", plan.ServiceActive)
		if force {
			fmt.Println("force:           true (unit will be rewritten even if unchanged)")
		}
		fmt.Println()
		fmt.Println("actions that would run:")
		for _, step := range plan.steps() {
			fmt.Printf("  - %s\n", step)
		}
		return
	}

	if plan.UnitChanged {
		// Use rewriteUnitWithRollback so a daemon-reload failure (e.g. a
		// syntax error introduced by a future template change that slipped
		// past unit tests) doesn't leave a broken unit file on disk. The
		// rollback path restores the previously-good bytes; the outer
		// fatalf still aborts the installer so enable/start don't run
		// against a systemd still in an error state.
		reloadErr := rewriteUnitWithRollback(
			systemdUnitPath,
			unit,
			string(existingBytes),
			existingErr,
			os.WriteFile,
			os.Remove,
			func() error { return run("systemctl", "daemon-reload") },
		)
		if reloadErr != nil {
			fatalf("%v\n\n%s", reloadErr, recoveryHint())
		}
	} else {
		fmt.Println("unit file unchanged; skipping daemon-reload")
	}

	// `enable` is idempotent on systemd — running it when already enabled
	// prints "already enabled" to stderr and exits 0. We always call it so
	// a half-installed prior state (unit on disk but not enabled) self-
	// heals on the next `naozhi install`.
	if err := run("systemctl", "enable", "naozhi"); err != nil {
		fatalf("systemctl enable naozhi: %v\n\n%s", err, recoveryHint())
	}

	// Pick the final hop based on plan:
	//   - not active           → start
	//   - active + unit changed → restart (so systemd re-reads the unit)
	//   - active + unit same    → no-op ("already running")
	switch {
	case !plan.ServiceActive:
		if err := run("systemctl", "start", "naozhi"); err != nil {
			fatalf("systemctl start naozhi: %v\n\n%s", err, recoveryHint())
		}
		fmt.Println("naozhi installed and started as systemd service.")
	case plan.UnitChanged:
		if err := run("systemctl", "restart", "naozhi"); err != nil {
			fatalf("systemctl restart naozhi: %v\n\n%s", err, recoveryHint())
		}
		fmt.Println("naozhi unit updated; service restarted.")
	default:
		fmt.Println("naozhi already installed and running; no changes.")
	}

	fmt.Println()
	fmt.Println("  Status:   sudo systemctl status naozhi")
	fmt.Println("  Logs:     sudo journalctl -u naozhi -f")
	fmt.Println("  Stop:     sudo systemctl stop naozhi")
	fmt.Println("  Remove:   sudo naozhi uninstall")
}

// steps renders the human-readable action list used by -dry-run. Order
// matches the effectful path in installSystemd so operators can grep the
// same command names.
func (p installSystemdPlan) steps() []string {
	var out []string
	if p.UnitChanged {
		out = append(out, "write unit file")
		out = append(out, "systemctl daemon-reload")
	} else {
		out = append(out, "skip: unit file unchanged")
	}
	out = append(out, "systemctl enable naozhi (idempotent)")
	switch {
	case !p.ServiceActive:
		out = append(out, "systemctl start naozhi")
	case p.UnitChanged:
		out = append(out, "systemctl restart naozhi")
	default:
		out = append(out, "skip: service active and unit unchanged")
	}
	return out
}

// recoveryHint is the operator-facing checklist printed on any systemctl
// failure so a half-installed state can be diagnosed without digging
// through journal logs. Centralised so the three failure branches above
// stay consistent.
func recoveryHint() string {
	return strings.Join([]string{
		"Recovery steps:",
		"  1. Inspect journal:   sudo journalctl -u naozhi --since '5 min ago'",
		"  2. Check unit file:   sudo cat " + systemdUnitPath,
		"  3. Remove if stuck:   sudo naozhi uninstall",
		"  4. Re-run install:    sudo naozhi install",
	}, "\n")
}

func uninstallSystemd() {
	if os.Getuid() != 0 {
		fatalf("systemd uninstall requires root. Run: sudo naozhi uninstall")
	}

	// Best-effort stop and disable; ignore errors if service doesn't exist.
	_ = run("systemctl", "stop", "naozhi")
	_ = run("systemctl", "disable", "naozhi")

	if err := os.Remove(systemdUnitPath); err != nil && !errors.Is(err, iofs.ErrNotExist) {
		fatalf("remove unit file: %v", err)
	}

	_ = run("systemctl", "daemon-reload")

	fmt.Println("naozhi service removed.")
}

