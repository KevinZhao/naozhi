package selfupdate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
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

// RestartServiceNoWait triggers a service restart and returns as soon as the
// restart job is accepted, WITHOUT polling for the unit to become active.
//
// This is the correct primitive for an IN-PROCESS auto-update: the caller is
// the very process being restarted, so waiting for `is-active` is meaningless
// — at the instant the restart job is queued the old process (us) is still
// "active", so a poll would falsely confirm success and then we'd be killed
// mid-confirmation. RestartService's waitServiceActive only makes sense for an
// EXTERNAL upgrader process (the `naozhi upgrade` CLI) confirming a separate
// daemon came up. systemd's Restart=always is what actually brings the new
// binary up here; our job ends at "restart triggered".
//
// A non-running service is a no-op (not an error).
func RestartServiceNoWait(ctx context.Context) error {
	switch runtime.GOOS {
	case "linux":
		return restartSystemdNoWait()
	case "darwin":
		return restartLaunchd()
	default:
		return fmt.Errorf("service restart not supported on %s — restart manually", runtime.GOOS)
	}
}

// restartSystemdNoWait issues `systemctl restart --no-block` and returns once
// the job is queued. No waitServiceActive — see RestartServiceNoWait.
func restartSystemdNoWait() error {
	if !ServiceRunning() {
		return nil
	}
	if out, err := exec.Command(resolveTrustedBin("systemctl"), "restart", "--no-block", "naozhi").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl restart --no-block naozhi: %w\n%s", err, out)
	}
	return nil
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
		return verifiedLaunchdLabel() != ""
	default:
		return false
	}
}

// LaunchdLabel is the launchd service label `naozhi install` writes into the
// plist. It is the FALLBACK label, not the authoritative one — a plist written
// by hand (or by an earlier naozhi version, or by a config-management tool)
// can carry any label at all, and the running process must honour whatever it
// was actually launched under. See launchdServiceLabel.
const LaunchdLabel = "com.naozhi.naozhi"

// xpcServiceNameEnv is the environment variable launchd injects into every
// process it spawns, set to that job's label.
const xpcServiceNameEnv = "XPC_SERVICE_NAME"

// launchdServiceLabel returns the label of the launchd job this process is
// running under.
//
// Reading it from the environment rather than assuming LaunchdLabel fixes a
// silent, load-bearing bug: a deployment whose plist uses any other label
// (observed in this project's own dev deployment: "com.naozhi.agent") made
// ServiceRunning() return false, which made every restart path — `naozhi
// upgrade`, auto-update mode "auto", and RestartServiceNoWait — quietly decide
// there was no service to restart and do nothing. No error, no warning; the
// operator was simply left with a staged binary that never applied.
//
// launchd sets XPC_SERVICE_NAME to the job label for the processes it spawns,
// so this is the authoritative value, obtained without shelling out.
//
// The fallback matters for the non-launchd case: `naozhi upgrade` run by hand
// from a terminal has no XPC_SERVICE_NAME, and there we keep the historical
// behaviour of assuming the label `naozhi install` would have written.
func launchdServiceLabel() string {
	if l := strings.TrimSpace(os.Getenv(xpcServiceNameEnv)); l != "" {
		return l
	}
	return LaunchdLabel
}

// verifiedLaunchdLabel returns the launchd label managing THIS binary, or ""
// when no such job can be confirmed.
//
// The verification is not paranoia — it closes a way to restart the wrong
// service. XPC_SERVICE_NAME is inherited, so a naozhi started by hand from a
// launchd-managed parent (Terminal.app is itself a launchd job) sees that
// parent's label. Acting on it unchecked would mean `launchctl kickstart -k
// gui/501/com.apple.Terminal` — restarting the operator's terminal instead of
// naozhi. And a bare `launchctl list <label>` succeeds there, so existence
// alone is not evidence.
//
// So we confirm the job actually runs our own executable. Cheap: one
// `launchctl list <label>` for a single known label, not a scan.
func verifiedLaunchdLabel() string {
	label := launchdServiceLabel()
	out, err := exec.Command(resolveTrustedBin("launchctl"), "list", label).Output()
	if err != nil || len(out) == 0 {
		return ""
	}
	self, err := SelfPath()
	if err != nil {
		return ""
	}
	if !launchdJobRunsPath(string(out), self) {
		return ""
	}
	return label
}

// launchdJobPathRe pulls the executable out of `launchctl list <label>` output.
// The plist-ish dump contains either
//
//	"Program" = "/path/to/bin";
//
// or, when the plist only sets ProgramArguments,
//
//	"ProgramArguments" = (
//	        "/path/to/bin";
//
// Matching `Program` first and falling back to the first ProgramArguments entry
// covers both, which matters because `naozhi install` writes ProgramArguments
// without a Program key.
var launchdJobPathRe = regexp.MustCompile(`"Program"\s*=\s*"([^"]+)"`)

var launchdJobArgv0Re = regexp.MustCompile(`"ProgramArguments"\s*=\s*\(\s*\n?\s*"([^"]+)"`)

// launchdJobRunsPath reports whether the job description refers to selfPath.
// Both sides are symlink-resolved so /usr/local/bin/naozhi and a symlink to it
// compare equal — SelfPath() already resolves, and the plist may not.
func launchdJobRunsPath(listOutput, selfPath string) bool {
	candidates := make([]string, 0, 2)
	if m := launchdJobPathRe.FindStringSubmatch(listOutput); len(m) == 2 {
		candidates = append(candidates, m[1])
	}
	if m := launchdJobArgv0Re.FindStringSubmatch(listOutput); len(m) == 2 {
		candidates = append(candidates, m[1])
	}
	if len(candidates) == 0 {
		// No executable in the dump: cannot confirm, so refuse. Failing closed
		// means we skip a restart we might have been able to do; failing open
		// means we might restart something else entirely.
		return false
	}
	for _, c := range candidates {
		if c == selfPath {
			return true
		}
		if resolved, err := filepath.EvalSymlinks(c); err == nil && resolved == selfPath {
			return true
		}
	}
	return false
}

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

// restartLaunchd restarts the naozhi launchd job.
//
// It uses `launchctl kickstart -k`, which asks launchd to terminate the job and
// start it again. That indirection is the whole point: the caller is normally
// the very process being restarted, so launchd — not us — has to be the one
// holding the restart across our death.
//
// The previous implementation was `launchctl unload <plist>` followed by
// `launchctl load -w <plist>`. That cannot work for a self-restart: `unload`
// removes the job whose process is making the call, so the `load` line runs (at
// best) in a race against our own SIGTERM and (at worst) never runs at all,
// leaving the service unloaded — stopped, not restarted. `kickstart -k` keeps
// the job registered in the domain throughout and mirrors the semantics of
// `systemctl restart --no-block` on the Linux side.
func restartLaunchd() error {
	// verifiedLaunchdLabel, not launchdServiceLabel: the label must be
	// confirmed to manage this binary before we hand it to kickstart -k.
	// See verifiedLaunchdLabel for what an unverified label can restart.
	label := verifiedLaunchdLabel()
	if label == "" {
		// No confirmed managing job — same "nothing to restart" semantics the
		// systemd path has when the unit is inactive.
		return nil
	}
	// gui/<uid> is the domain for a LaunchAgent, which is what `naozhi
	// install` writes (~/Library/LaunchAgents). A LaunchDaemon would need
	// `system/`; if daemon installs are ever supported, branch on the plist
	// location here.
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), label)
	if out, err := exec.Command(resolveTrustedBin("launchctl"), "kickstart", "-k", target).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl kickstart -k %s: %w\n%s", target, err, out)
	}
	return nil
}
