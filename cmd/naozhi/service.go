package main

import (
	"errors"
	"flag"
	"fmt"
	"html"
	iofs "io/fs"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"runtime"

	"github.com/naozhi/naozhi/internal/selfupdate"
)

const (
	systemdUnitPath = "/etc/systemd/system/naozhi.service"
	// systemdUnitBackupSuffix is appended to systemdUnitPath to form the
	// rollback target. Kept next to the unit (not /tmp) so a crashing
	// installer leaves the evidence in the canonical place operators
	// check first. Cleared on successful install.
	systemdUnitBackupSuffix = ".naozhi-install.bak"
)

// launchdLabel and launchdPlistPath are authoritative in internal/selfupdate
// so that naozhi install and naozhi upgrade always operate on the same plist.
const launchdLabel = selfupdate.LaunchdLabel

func launchdPlistPath() string {
	return selfupdate.LaunchdPlistPath()
}

// serviceUser returns the effective user and home directory for the service.
// Under sudo, it returns the original invoking user.
func serviceUser() (user, home string) {
	if su := os.Getenv("SUDO_USER"); su != "" {
		// POSIX login.defs LOGIN_NAME_MAX is 256; reject longer to bound argv/env growth.
		if len(su) > 256 {
			fatalf("SUDO_USER too long: %d bytes", len(su))
		}
		// Validate username format to prevent injection into systemd unit files.
		for _, c := range su {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
				fatalf("SUDO_USER contains invalid characters: %q", su)
			}
		}
		user = su
		// Resolve home via os/user.Lookup so the install path does not
		// depend on a `getent` binary being present on PATH and does
		// not pay the fork+exec round-trip every install/uninstall
		// cycle. os/user falls through nsswitch.conf entries on glibc
		// hosts (compat -> files -> sssd -> ldap -> ...), so it
		// already covers the LDAP/SSSD-only home directories that
		// motivated the original getent shellout. (#391)
		//
		// Fallback to /home/<su> mirrors the prior behaviour: a
		// Lookup failure (su not in any nsswitch backend, or a pure
		// container with only minimal /etc/passwd entries) keeps the
		// installer functional rather than aborting on the most
		// common deployment path.
		if u, err := osuser.Lookup(su); err == nil && u.HomeDir != "" {
			home = u.HomeDir
		}
		if home == "" {
			home = filepath.Join("/home", su)
		}
		return
	}
	user = os.Getenv("USER")
	var err error
	home, err = os.UserHomeDir()
	if err != nil || home == "" {
		fatalf("UserHomeDir: %v (home=%q); set $HOME or run without sudo", err, home)
	}
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
	if err := fs.Parse(args); err != nil {
		fatalf("parse install args: %v", err)
	}

	if *configPath == "" {
		_, home := serviceUser()
		*configPath = filepath.Join(home, ".naozhi", "config.yaml")
	}

	absConfig, err := filepath.Abs(*configPath)
	if err != nil {
		fatalf("resolve config path: %v", err)
	}
	if _, err := os.Stat(absConfig); errors.Is(err, iofs.ErrNotExist) {
		fatalf("config not found: %s\nRun 'naozhi setup' first to generate config.", absConfig)
	}

	binary, err := os.Executable()
	if err != nil {
		fatalf("find binary path: %v", err)
	}
	// EvalSymlinks 失败时回退到原始路径（典型场景：binary 通过非符号链接路径运行）
	if resolved, err := filepath.EvalSymlinks(binary); err == nil {
		binary = resolved
	}

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

	if err := os.Remove(plistPath); err != nil && !errors.Is(err, iofs.ErrNotExist) {
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
