package selfupdate

import (
	"fmt"
	"os/exec"
	"runtime"
)

// RestartService attempts to restart the naozhi system service after a
// binary replacement. On Linux it calls systemctl; on macOS it reloads
// the launchd plist. A non-running service is a no-op (not an error).
func RestartService() error {
	switch runtime.GOOS {
	case "linux":
		return restartSystemd()
	case "darwin":
		return restartLaunchd()
	default:
		return fmt.Errorf("service restart not supported on %s — restart manually", runtime.GOOS)
	}
}

// ServiceRunning reports whether the naozhi service is currently active.
func ServiceRunning() bool {
	switch runtime.GOOS {
	case "linux":
		return exec.Command("systemctl", "is-active", "--quiet", "naozhi").Run() == nil
	case "darwin":
		out, err := exec.Command("launchctl", "list", launchdLabel).Output()
		return err == nil && len(out) > 0
	default:
		return false
	}
}

// LaunchdLabel is the launchd service label used by both naozhi install and
// naozhi upgrade to ensure they operate on the same plist.
const LaunchdLabel = "com.naozhi.naozhi"

// keep unexported alias so internal helpers stay readable
const launchdLabel = LaunchdLabel

func restartSystemd() error {
	// Only restart if the unit is currently active — avoid starting a stopped
	// service as a side-effect of upgrade.
	if !ServiceRunning() {
		return nil
	}
	if out, err := exec.Command("systemctl", "restart", "naozhi").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl restart naozhi: %w\n%s", err, out)
	}
	return nil
}

func restartLaunchd() error {
	if !ServiceRunning() {
		return nil
	}
	plistPath := launchdPlistPath()
	if out, err := exec.Command("launchctl", "unload", plistPath).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl unload: %w\n%s", err, out)
	}
	if out, err := exec.Command("launchctl", "load", "-w", plistPath).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load: %w\n%s", err, out)
	}
	return nil
}
