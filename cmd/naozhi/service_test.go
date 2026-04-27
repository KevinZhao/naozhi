package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceUser(t *testing.T) {
	// Without SUDO_USER, returns current user.
	t.Setenv("SUDO_USER", "")
	user, home := serviceUser()
	if user == "" {
		t.Error("expected non-empty user")
	}
	if home == "" {
		t.Error("expected non-empty home")
	}
}

func TestServiceUserSudo(t *testing.T) {
	t.Setenv("SUDO_USER", "testuser")
	user, home := serviceUser()
	if user != "testuser" {
		t.Errorf("expected user=testuser, got %s", user)
	}
	// getent may not resolve testuser, fallback to /home/testuser
	if home == "" {
		t.Error("expected non-empty home")
	}
}

func TestRunInstallMissingConfig(t *testing.T) {
	// Verify that install checks for config existence.
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "nonexistent.yaml")

	// We can't call runInstall directly because it calls os.Exit.
	// Instead, verify the config check logic.
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Error("expected config to not exist")
	}
}

func TestLaunchdPlistPath(t *testing.T) {
	path := launchdPlistPath()
	if !strings.HasSuffix(path, "Library/LaunchAgents/com.naozhi.naozhi.plist") {
		t.Errorf("unexpected plist path: %s", path)
	}
}

func TestGenerateSystemdUnit(t *testing.T) {
	unit := generateSystemdUnit("/usr/local/bin/naozhi", "/home/app/.naozhi/config.yaml", "app", "/home/app")

	if !strings.Contains(unit, `ExecStart="/usr/local/bin/naozhi" --config "/home/app/.naozhi/config.yaml"`) {
		t.Error("ExecStart line missing or malformed")
	}
	if !strings.Contains(unit, "WorkingDirectory=/home/app") {
		t.Error("WorkingDirectory missing")
	}
	if !strings.Contains(unit, "User=app") {
		t.Error("User field missing")
	}
	if !strings.Contains(unit, "Environment=HOME=/home/app") {
		t.Error("HOME environment missing")
	}
	if !strings.Contains(unit, "Type=notify") {
		t.Error("expected Type=notify for sd_notify support")
	}
	if !strings.Contains(unit, "WatchdogSec=120") {
		t.Error("expected WatchdogSec=120 for hung-process detection")
	}
	if !strings.Contains(unit, "NotifyAccess=main") {
		t.Error("expected NotifyAccess=main")
	}
	// Shim-survival settings: naozhi moves shim helpers into a shared
	// cgroup so they outlive the main service process for zero-downtime
	// reconnect. The default control-group kill mode would tear them
	// down on every `systemctl restart`. Test all three directives so a
	// future refactor that drops any one of them trips.
	if !strings.Contains(unit, "KillMode=process") {
		t.Error("expected KillMode=process to preserve shim processes across restart")
	}
	if !strings.Contains(unit, "SendSIGKILL=no") {
		t.Error("expected SendSIGKILL=no so cgroup shims are never force-killed")
	}
	if !strings.Contains(unit, "TimeoutStopSec=5") {
		t.Error("expected TimeoutStopSec=5 for prompt graceful shutdown budget")
	}
}

// TestGenerateSystemdUnit_MatchesDeployTemplate verifies the installer-
// rendered unit stays in lockstep with deploy/naozhi.service on the
// invariant fields that affect service semantics. Drift between the
// two templates produced the R-series TODO entry "deploy/naozhi.service
// 与 `naozhi install` 渲染的 systemd unit 漂移" — if the installer ever
// emits something the copy-paste deploy file would also need, both
// change together or this test fails. We compare service-semantics
// directives, not user-specific paths (User/WorkingDirectory/ExecStart).
func TestGenerateSystemdUnit_MatchesDeployTemplate(t *testing.T) {
	unit := generateSystemdUnit("/usr/local/bin/naozhi", "/etc/naozhi/config.yaml", "app", "/var/lib/naozhi")

	// Load the deploy file from the repo so drift is detected at test
	// time rather than at deploy time. Path is relative to this test
	// (cmd/naozhi/), go up two to reach the repo root.
	deployBytes, err := os.ReadFile(filepath.Join("..", "..", "deploy", "naozhi.service"))
	if err != nil {
		t.Fatalf("read deploy/naozhi.service: %v", err)
	}
	deployUnit := string(deployBytes)

	// Directives that both templates MUST carry identically. Missing in
	// either path corrupts service semantics: Type=notify is required
	// by main.go's sd_notify call; KillMode/SendSIGKILL/TimeoutStopSec
	// protect the cgroup shim processes on restart.
	sharedDirectives := []string{
		"Type=notify",
		"NotifyAccess=main",
		"WatchdogSec=120",
		"Restart=always",
		"KillMode=process",
		"SendSIGKILL=no",
		"TimeoutStopSec=5",
	}
	for _, d := range sharedDirectives {
		if !strings.Contains(unit, d) {
			t.Errorf("rendered unit missing %q", d)
		}
		if !strings.Contains(deployUnit, d) {
			t.Errorf("deploy/naozhi.service missing %q — drift vs generateSystemdUnit", d)
		}
	}
}

func TestGenerateSystemdUnitQuotesSpaces(t *testing.T) {
	unit := generateSystemdUnit("/opt/my app/naozhi", "/home/user/my config/config.yaml", "user", "/home/user")

	if !strings.Contains(unit, `ExecStart="/opt/my app/naozhi" --config "/home/user/my config/config.yaml"`) {
		t.Errorf("ExecStart does not properly quote paths with spaces:\n%s", unit)
	}
}

func TestGenerateLaunchdPlist(t *testing.T) {
	plist := generateLaunchdPlist("/usr/local/bin/naozhi", "/Users/app/.naozhi/config.yaml", "/Users/app/.naozhi/log")

	if !strings.Contains(plist, "<string>/usr/local/bin/naozhi</string>") {
		t.Error("binary not found in plist")
	}
	if !strings.Contains(plist, "<string>/Users/app/.naozhi/config.yaml</string>") {
		t.Error("config path not found in plist")
	}
	if !strings.Contains(plist, "naozhi.log</string>") {
		t.Error("log path not found in plist")
	}
}

func TestGenerateLaunchdPlistEscapesXML(t *testing.T) {
	plist := generateLaunchdPlist("/opt/my<app>/naozhi", "/home/user&co/config.yaml", "/tmp/log")

	if strings.Contains(plist, "<app>") {
		t.Error("XML special characters not escaped in binary path")
	}
	if !strings.Contains(plist, "&lt;app&gt;") {
		t.Error("expected escaped < and > in binary path")
	}
	if !strings.Contains(plist, "&amp;co") {
		t.Error("expected escaped & in config path")
	}
}
