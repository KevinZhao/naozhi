package selfupdate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"
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
func RestartService(ctx context.Context) error {
	switch runtime.GOOS {
	case "linux":
		return restartSystemd(ctx)
	case "darwin":
		return restartLaunchd()
	default:
		return fmt.Errorf("service restart not supported on %s — restart manually", runtime.GOOS)
	}
}

// systemdUnitActive reports whether `systemctl is-active --quiet naozhi`
// exits 0. Indirected through a var so tests can simulate a unit that is
// "activating" for a while and then flips to "active". Production wiring is
// the real systemctl call.
//
// Test hygiene: this is mutable package state with no lock. Tests that swap
// it (and tests that exercise ServiceRunning/waitServiceActive, which read
// it) MUST NOT call t.Parallel(), or they race each other. Same convention
// the download-helper tests in this package already follow.
var systemdUnitActive = func() bool {
	return exec.Command(resolveTrustedBin("systemctl"), "is-active", "--quiet", "naozhi").Run() == nil
}

// ServiceRunning reports whether the naozhi service is currently active.
func ServiceRunning() bool {
	switch runtime.GOOS {
	case "linux":
		return systemdUnitActive()
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

// restartConfirmTimeout bounds how long restartSystemd waits for the unit to
// report active again after issuing an async restart. naozhi's unit is
// Type=notify, so "active" means the process called sd_notify(READY=1) — which
// it does right after the HTTP server starts listening. On a loaded host the
// cold-start replay (shim reconnect, history, cron) can take a while, so this
// is generous; it only bounds the *confirmation*, not the restart itself.
var restartConfirmTimeout = 3 * time.Minute

// restartConfirmInterval is how often waitServiceActive polls is-active.
var restartConfirmInterval = 2 * time.Second

func restartSystemd(ctx context.Context) error {
	// Only restart if the unit is currently active — avoid starting a stopped
	// service as a side-effect of upgrade.
	if !ServiceRunning() {
		return nil
	}
	// --no-block: return as soon as systemd accepts the job, instead of
	// blocking until the unit reaches "active". A synchronous `systemctl
	// restart` waits for sd_notify(READY=1) up to TimeoutStartSec; naozhi's
	// cold start can exceed that on a loaded host, so the blocking form
	// reports a spurious non-zero exit even though the service comes up fine.
	// That false failure is what made `naozhi upgrade` roll back a healthy
	// v0.0.27 binary. We confirm liveness ourselves below instead.
	if out, err := exec.Command(resolveTrustedBin("systemctl"), "restart", "--no-block", "naozhi").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl restart --no-block naozhi: %w\n%s", err, out)
	}
	return waitServiceActive(ctx, restartConfirmTimeout)
}

// waitServiceActive polls `systemctl is-active` until the unit is active, the
// timeout elapses, or ctx is cancelled. A timeout/cancel is reported as an
// error so the caller can surface it, but callers MUST NOT treat it as a
// reason to roll back the binary: the new binary is already verified and
// executable, and systemd's Restart=always will keep bringing it up. A slow
// confirmation is not a corrupt install.
func waitServiceActive(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		// is-active exits 0 only in the "active" state; "activating"
		// (Type=notify before READY=1) and "failed" both exit non-zero, so a
		// true result here means the unit finished starting.
		if systemdUnitActive() {
			return nil
		}
		// Clamp the sleep to whatever time is left so a large poll interval
		// can never push the return past the deadline (and a timeout shorter
		// than one interval is still honored).
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("service did not become active within %s after restart (it may still be starting; check `systemctl status naozhi`)", timeout)
		}
		sleep := restartConfirmInterval
		if sleep > remaining {
			sleep = remaining
		}
		t := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("restart confirmation interrupted: %w", ctx.Err())
		case <-t.C:
		}
	}
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
