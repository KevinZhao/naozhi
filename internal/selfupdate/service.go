package selfupdate

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
)

// resolveTrustedBin returns an absolute path to a known system binary,
// preferring the canonical /usr/bin location (and /bin on systems with
// usrmerge) before falling back to exec.LookPath. Self-update is one of
// the few paths where naozhi shells out as root with privilege; using a
// PATH lookup means a poisoned PATH (admin misconfig, or a local
// privesc that prepends a writable dir) lets an attacker inject a
// replacement binary that runs in the upgrade context. R237-SEC-6 / #652.
//
// The resolved path is cached per name via a sync.Once so resolution
// happens at most once per process.
func resolveTrustedBin(name string) string {
	c := trustedBinCache(name)
	c.once.Do(func() {
		// Prefer canonical absolute paths. systemd is shipped under
		// /usr/bin on every modern distro (Amazon Linux, Debian/Ubuntu
		// post-usrmerge, RHEL/Fedora, Arch). /bin and /usr/local/sbin
		// are tried only as conservative fallbacks before LookPath.
		for _, p := range []string{
			"/usr/bin/" + name,
			"/bin/" + name,
			"/usr/sbin/" + name,
			"/sbin/" + name,
		} {
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				c.path = p
				return
			}
		}
		// Last resort: PATH lookup. Operators with a non-standard
		// install layout (e.g. /opt/...) still get a working upgrade,
		// at the cost of trusting their PATH. The Stat-first sweep
		// above catches the overwhelming majority of installs and
		// closes the PATH-poisoning vector for them.
		if p, err := exec.LookPath(name); err == nil {
			c.path = p
			return
		}
		c.path = name // bare name; exec.Command will surface the failure
	})
	return c.path
}

type binCacheEntry struct {
	once sync.Once
	path string
}

var (
	binCacheMu sync.Mutex
	binCache   = map[string]*binCacheEntry{}
)

func trustedBinCache(name string) *binCacheEntry {
	binCacheMu.Lock()
	defer binCacheMu.Unlock()
	if e, ok := binCache[name]; ok {
		return e
	}
	e := &binCacheEntry{}
	binCache[name] = e
	return e
}

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
		return exec.Command(resolveTrustedBin("systemctl"), "is-active", "--quiet", "naozhi").Run() == nil
	case "darwin":
		out, err := exec.Command(resolveTrustedBin("launchctl"), "list", launchdLabel).Output()
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
	if out, err := exec.Command(resolveTrustedBin("systemctl"), "restart", "naozhi").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl restart naozhi: %w\n%s", err, out)
	}
	return nil
}

func restartLaunchd() error {
	if !ServiceRunning() {
		return nil
	}
	plistPath := launchdPlistPath()
	launchctl := resolveTrustedBin("launchctl")
	if out, err := exec.Command(launchctl, "unload", plistPath).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl unload: %w\n%s", err, out)
	}
	if out, err := exec.Command(launchctl, "load", "-w", plistPath).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load: %w\n%s", err, out)
	}
	return nil
}
