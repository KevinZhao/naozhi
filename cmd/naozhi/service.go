package main

import (
	"flag"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	systemdUnitPath = "/etc/systemd/system/naozhi.service"
	launchdLabel    = "com.naozhi.naozhi"
)

func launchdPlistPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		fatalf("determine home directory: %v", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}

// serviceUser returns the effective user and home directory for the service.
// Under sudo, it returns the original invoking user.
func serviceUser() (user, home string) {
	if su := os.Getenv("SUDO_USER"); su != "" {
		// Validate username format to prevent injection into systemd unit files.
		for _, c := range su {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
				fatalf("SUDO_USER contains invalid characters: %q", su)
			}
		}
		user = su
		// Resolve home via getent (works on Linux)
		if out, err := exec.Command("getent", "passwd", su).Output(); err == nil {
			fields := strings.Split(strings.TrimSpace(string(out)), ":")
			if len(fields) >= 6 {
				home = fields[5]
			}
		}
		if home == "" {
			home = filepath.Join("/home", su)
		}
		return
	}
	user = os.Getenv("USER")
	home, _ = os.UserHomeDir()
	return
}

func runInstall(args []string) {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	configPath := fs.String("config", "", "config file path (default ~/.naozhi/config.yaml)")
	dryRun := fs.Bool("dry-run", false, "print what would change without writing unit file or invoking systemctl")
	// -force overrides the idempotency shortcut that skips daemon-reload
	// when the rendered unit matches the on-disk unit. Use when the unit
	// file was hand-edited and must be restored, or when you want to
	// re-run daemon-reload + restart after a binary swap with no unit
	// churn. Orthogonal to -dry-run (the pair prints the forced plan).
	force := fs.Bool("force", false, "rewrite unit file and restart even if nothing changed (systemd only)")
	fs.Parse(args)

	if *configPath == "" {
		_, home := serviceUser()
		*configPath = filepath.Join(home, ".naozhi", "config.yaml")
	}

	absConfig, err := filepath.Abs(*configPath)
	if err != nil {
		fatalf("resolve config path: %v", err)
	}
	if _, err := os.Stat(absConfig); os.IsNotExist(err) {
		fatalf("config not found: %s\nRun 'naozhi setup' first to generate config.", absConfig)
	}

	binary, err := os.Executable()
	if err != nil {
		fatalf("find binary path: %v", err)
	}
	binary, _ = filepath.EvalSymlinks(binary)

	switch runtime.GOOS {
	case "linux":
		installSystemd(binary, absConfig, *dryRun, *force)
	case "darwin":
		if *dryRun {
			fmt.Println("note: -dry-run is a no-op on darwin (launchd path has no idempotency checks yet)")
		}
		if *force {
			fmt.Println("note: -force is a no-op on darwin (launchd path always writes plist + reloads)")
		}
		installLaunchd(binary, absConfig)
	default:
		fatalf("unsupported OS: %s (supported: linux, darwin)", runtime.GOOS)
	}
}

func runUninstall(_ []string) {
	switch runtime.GOOS {
	case "linux":
		uninstallSystemd()
	case "darwin":
		uninstallLaunchd()
	default:
		fatalf("unsupported OS: %s", runtime.GOOS)
	}
}

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
			fatalf("write unit file: %v", err)
		}
		if err := run("systemctl", "daemon-reload"); err != nil {
			fatalf("systemctl daemon-reload: %v\n\n%s", err, recoveryHint())
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

	if err := os.Remove(systemdUnitPath); err != nil && !os.IsNotExist(err) {
		fatalf("remove unit file: %v", err)
	}

	_ = run("systemctl", "daemon-reload")

	fmt.Println("naozhi service removed.")
}

// --- launchd (macOS) ---

func generateLaunchdPlist(binary, configPath, logDir string) string {
	xesc := html.EscapeString
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>--config</string>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s/naozhi.log</string>
	<key>StandardErrorPath</key>
	<string>%s/naozhi.err</string>
</dict>
</plist>
`, launchdLabel, xesc(binary), xesc(configPath), xesc(logDir), xesc(logDir))
}

func installLaunchd(binary, configPath string) {
	home, err := os.UserHomeDir()
	if err != nil {
		fatalf("determine home directory: %v", err)
	}
	logDir := filepath.Join(home, ".naozhi", "log")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		fatalf("create log dir: %v", err)
	}

	plist := generateLaunchdPlist(binary, configPath, logDir)

	plistPath := launchdPlistPath()
	if err := os.MkdirAll(filepath.Dir(plistPath), 0755); err != nil {
		fatalf("create LaunchAgents dir: %v", err)
	}

	// Unload existing if present (ignore errors).
	if _, err := os.Stat(plistPath); err == nil {
		_ = run("launchctl", "unload", plistPath)
	}

	if err := os.WriteFile(plistPath, []byte(plist), 0644); err != nil {
		fatalf("write plist: %v", err)
	}

	if err := run("launchctl", "load", "-w", plistPath); err != nil {
		fatalf("launchctl load: %v", err)
	}

	fmt.Println("naozhi installed and started as launchd agent.")
	fmt.Println()
	fmt.Printf("  Logs:     tail -f %s/naozhi.log\n", logDir)
	fmt.Println("  Stop:     launchctl unload " + plistPath)
	fmt.Println("  Remove:   naozhi uninstall")
}

func uninstallLaunchd() {
	plistPath := launchdPlistPath()

	if _, err := os.Stat(plistPath); err == nil {
		_ = run("launchctl", "unload", plistPath)
	}

	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		fatalf("remove plist: %v", err)
	}

	fmt.Println("naozhi service removed.")
}

// --- helpers ---

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", args...)
	os.Exit(1)
}
