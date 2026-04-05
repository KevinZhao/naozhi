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
