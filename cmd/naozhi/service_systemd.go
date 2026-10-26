// Linux/systemd 安装逻辑；launchd（macOS）与共用 helper 在 service.go。
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
	// Type=notify + WatchdogSec: main.go sends READY=1 and WATCHDOG pings.
	// KillMode=process + SendSIGKILL=no: shims live in their own cgroup and
	// must survive restarts; the default control-group kill would SIGKILL
	// every shim and lose in-flight CLI sessions. Must match deploy/naozhi.service.
	return fmt.Sprintf(`[Unit]
Description=naozhi - Claude Code IM Gateway
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=60s
StartLimitBurst=5

[Service]
Type=notify
NotifyAccess=main
WatchdogSec=120
ExecStart="%s" --config "%s"
ExecReload=/bin/kill -HUP $MAINPID
WorkingDirectory=%s
Restart=always
RestartSec=5
User=%s
Environment=HOME=%s
KillMode=process
SendSIGKILL=no
TimeoutStopSec=5

[Install]
WantedBy=multi-user.target
`, binary, configPath, home, user, home)
}

// installSystemdPlan classifies the actions needed to converge the host into
// the desired systemd state; pure data so tests can assert the decision matrix.
type installSystemdPlan struct {
	// UnitChanged: rendered unit differs from disk (or none on disk); drives
	// the rewrite + daemon-reload.
	UnitChanged bool
	// ServiceActive mirrors `systemctl is-active naozhi`; picks start vs restart.
	ServiceActive bool
}

// planInstallSystemd derives the plan from rendered-vs-existing unit bytes and
// service state. force=true sets UnitChanged regardless of byte-equality so a
// rerun still triggers daemon-reload + restart.
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

// systemctlIsActive reports whether `systemctl is-active <name>` exits 0;
// stubbable in tests. Any non-zero exit means "not active" (safe start branch).
var systemctlIsActive = func(name string) bool {
	cmd := exec.Command("systemctl", "is-active", "--quiet", name)
	return cmd.Run() == nil
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
		if err := os.WriteFile(systemdUnitPath, []byte(unit), 0644); err != nil {
			fatalf("write unit file: %v\n\n%s", err, recoveryHint())
		}
		if err := run("systemctl", "daemon-reload"); err != nil {
			fatalf("systemctl daemon-reload: %v\n\n%s", err, recoveryHint())
		}
	} else {
		fmt.Println("unit file unchanged; skipping daemon-reload")
	}

	// `enable` is idempotent; always run it so a half-installed state
	// (unit on disk but not enabled) self-heals.
	if err := run("systemctl", "enable", "naozhi"); err != nil {
		fatalf("systemctl enable naozhi: %v\n\n%s", err, recoveryHint())
	}

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

// steps renders the -dry-run action list in the same order as installSystemd.
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

// recoveryHint is the operator checklist printed on any systemctl failure.
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

	// Best-effort; the service may not exist.
	_ = run("systemctl", "stop", "naozhi")
	_ = run("systemctl", "disable", "naozhi")

	if err := os.Remove(systemdUnitPath); err != nil && !errors.Is(err, iofs.ErrNotExist) {
		fatalf("remove unit file: %v", err)
	}

	_ = run("systemctl", "daemon-reload")

	fmt.Println("naozhi service removed.")
}
