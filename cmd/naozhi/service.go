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
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
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
		fatalf("config not found: %s\nRun 'naozhi init' first to generate config.", absConfig)
	}

	binary, err := os.Executable()
	if err != nil {
		fatalf("find binary path: %v", err)
	}
	binary, _ = filepath.EvalSymlinks(binary)

	switch runtime.GOOS {
	case "linux":
		installSystemd(binary, absConfig)
	case "darwin":
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
	return fmt.Sprintf(`[Unit]
Description=naozhi - Claude Code IM Gateway
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart="%s" --config "%s"
WorkingDirectory=%s
Restart=always
RestartSec=5
StartLimitInterval=60s
StartLimitBurst=5
User=%s
Environment=HOME=%s

[Install]
WantedBy=multi-user.target
`, binary, configPath, home, user, home)
}

func installSystemd(binary, configPath string) {
	if os.Getuid() != 0 {
		fatalf("systemd install requires root. Run: sudo naozhi install")
	}

	user, home := serviceUser()
	unit := generateSystemdUnit(binary, configPath, user, home)

	if err := os.WriteFile(systemdUnitPath, []byte(unit), 0644); err != nil {
		fatalf("write unit file: %v", err)
	}

	cmds := [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", "naozhi"},
		{"systemctl", "start", "naozhi"},
	}
	for _, c := range cmds {
		if err := run(c[0], c[1:]...); err != nil {
			fatalf("%s: %v", strings.Join(c, " "), err)
		}
	}

	fmt.Println("naozhi installed and started as systemd service.")
	fmt.Println()
	fmt.Println("  Status:   sudo systemctl status naozhi")
	fmt.Println("  Logs:     sudo journalctl -u naozhi -f")
	fmt.Println("  Stop:     sudo systemctl stop naozhi")
	fmt.Println("  Remove:   sudo naozhi uninstall")
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
