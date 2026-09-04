package selfupdate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// resolveTrustedBin returns an absolute path to a system binary, preferring
// canonical locations over exec.LookPath: self-update shells out with
// privilege, and a poisoned PATH could inject a replacement binary (#652).
// Resolution is cached per name.
func resolveTrustedBin(name string) string {
	c := trustedBinCache(name)
	c.once.Do(func() {
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
		// Last resort for non-standard layouts, at the cost of trusting PATH.
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

// RestartServiceNoWait triggers a restart and returns once the job is accepted,
// WITHOUT polling for active. It is the IN-PROCESS primitive: the caller is
// the process being restarted, so an is-active poll would see us still active,
// falsely confirm, and be killed mid-confirmation; Restart=always brings the
// new binary up. No-op when the service does not manage THIS process. Indirected
// through restartServiceNoWaitFn so tests can refuse the (self-killing) call.
func RestartServiceNoWait(ctx context.Context) error {
	return restartServiceNoWaitFn(ctx)
}

// restartServiceNoWaitFn is the injectable implementation of
// RestartServiceNoWait; tests that swap it must not run in parallel.
var restartServiceNoWaitFn = restartServiceNoWait

func restartServiceNoWait(context.Context) error {
	switch runtime.GOOS {
	case "linux":
		return restartSystemdNoWait()
	case "darwin":
		return restartLaunchd()
	default:
		return fmt.Errorf("service restart not supported on %s — restart manually", runtime.GOOS)
	}
}

// restartSystemdNoWait issues `systemctl restart --no-block`. Gated on
// ServiceManagesThisProcess: from a process the unit does NOT run it would
// restart the system service and leave the caller stuck on "restarting".
func restartSystemdNoWait() error {
	if !ServiceManagesThisProcess() {
		return nil
	}
	if out, err := exec.Command(resolveTrustedBin("systemctl"), "restart", "--no-block", "naozhi").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl restart --no-block naozhi: %w\n%s", err, out)
	}
	return nil
}

// systemdUnitActive reports whether `systemctl is-active --quiet naozhi` exits
// 0. Indirected for tests; mutable package state with no lock, so tests that
// swap or read it MUST NOT call t.Parallel().
var systemdUnitActive = func() bool {
	return exec.Command(resolveTrustedBin("systemctl"), "is-active", "--quiet", "naozhi").Run() == nil
}

// ServiceRunning reports whether A naozhi service is active on this host — the
// EXTERNAL upgrader's question. It says nothing about whether that service
// runs the calling process; in-process callers use ServiceManagesThisProcess.
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

// systemdMainPID returns the unit's MainPID, or 0 when unknown/inactive.
// ExecStart runs the naozhi binary directly, so MainPID IS the server process.
// Indirected for tests (same no-t.Parallel rule as systemdUnitActive).
var systemdMainPID = func() int {
	out, err := exec.Command(resolveTrustedBin("systemctl"), "show", "naozhi", "-p", "MainPID", "--value").Output()
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// ServiceManagesThisProcess reports whether the service manager runs THIS
// process — i.e. whether restarting "naozhi" restarts us. A hand-started second
// naozhi on a host with an active unit would otherwise restart the SYSTEM
// service and park itself on "restarting" forever. Linux: unit active AND
// MainPID == our pid; darwin: verifiedLaunchdLabel already checks the executable.
func ServiceManagesThisProcess() bool {
	switch runtime.GOOS {
	case "linux":
		return systemdUnitActive() && systemdMainPID() == os.Getpid()
	case "darwin":
		return verifiedLaunchdLabel() != ""
	default:
		return false
	}
}

// LaunchdLabel is the label `naozhi install` writes into the plist — the
// FALLBACK, not the authority: a hand-written plist can carry any label and
// the running process must honour the one it was launched under.
const LaunchdLabel = "com.naozhi.naozhi"

// xpcServiceNameEnv is set by launchd on every process it spawns to the job label.
const xpcServiceNameEnv = "XPC_SERVICE_NAME"

// launchdServiceLabel returns the label of the launchd job this process runs
// under (XPC_SERVICE_NAME, authoritative and free), falling back to
// LaunchdLabel for a non-launchd context such as `naozhi upgrade` in a shell.
// Assuming the constant made every restart path silently no-op on
// deployments whose plist uses another label.
func launchdServiceLabel() string {
	if l := strings.TrimSpace(os.Getenv(xpcServiceNameEnv)); l != "" {
		return l
	}
	return LaunchdLabel
}

// verifiedLaunchdLabel returns the launchd label managing THIS binary, or ""
// when unconfirmed. XPC_SERVICE_NAME is inherited, so a naozhi started from
// Terminal.app sees com.apple.Terminal; acting on it would kickstart the
// operator's terminal. Hence confirm the job runs our own executable.
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

// launchdJobPathRe / launchdJobArgv0Re pull the executable out of `launchctl
// list <label>` output: `"Program" = "/path";` or the first ProgramArguments
// entry (`naozhi install` writes only ProgramArguments).
var launchdJobPathRe = regexp.MustCompile(`"Program"\s*=\s*"([^"]+)"`)

var launchdJobArgv0Re = regexp.MustCompile(`"ProgramArguments"\s*=\s*\(\s*\n?\s*"([^"]+)"`)

// launchdJobRunsPath reports whether the job description refers to selfPath
// (symlink-resolved on both sides; the plist may not be).
func launchdJobRunsPath(listOutput, selfPath string) bool {
	candidates := make([]string, 0, 2)
	if m := launchdJobPathRe.FindStringSubmatch(listOutput); len(m) == 2 {
		candidates = append(candidates, m[1])
	}
	if m := launchdJobArgv0Re.FindStringSubmatch(listOutput); len(m) == 2 {
		candidates = append(candidates, m[1])
	}
	if len(candidates) == 0 {
		// Fail closed: failing open could restart something else entirely.
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

// restartConfirmTimeout bounds how long restartSystemd waits for the
// Type=notify unit to report active again (READY=1 after a possibly slow
// cold-start replay); it bounds the confirmation only, not the restart.
var restartConfirmTimeout = 3 * time.Minute

// restartConfirmInterval is how often waitServiceActive polls is-active.
var restartConfirmInterval = 2 * time.Second

func restartSystemd(ctx context.Context) error {
	// Never start a stopped service as an upgrade side-effect.
	if !ServiceRunning() {
		return nil
	}
	// --no-block: a synchronous restart waits for READY=1 up to
	// TimeoutStartSec and reports a spurious failure on a slow cold start;
	// liveness is confirmed below instead.
	if out, err := exec.Command(resolveTrustedBin("systemctl"), "restart", "--no-block", "naozhi").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl restart --no-block naozhi: %w\n%s", err, out)
	}
	return waitServiceActive(ctx, restartConfirmTimeout)
}

// waitServiceActive polls is-active until active, timeout or ctx cancel. A
// timeout is an error to surface but MUST NOT trigger a binary rollback: the
// binary is verified and Restart=always keeps bringing it up.
func waitServiceActive(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		// is-active exits 0 only for "active" ("activating" and "failed" do not).
		if systemdUnitActive() {
			return nil
		}
		// Clamp so a large interval never overshoots the deadline.
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

// restartLaunchd restarts the naozhi launchd job via `launchctl kickstart -k`:
// launchd, not the dying caller, must own the restart. An unload+load pair
// cannot self-restart (unload removes the job making the call).
func restartLaunchd() error {
	// Must be the VERIFIED label — see verifiedLaunchdLabel.
	label := verifiedLaunchdLabel()
	if label == "" {
		return nil
	}
	// gui/<uid> is the LaunchAgent domain `naozhi install` writes to; a
	// LaunchDaemon would need `system/`.
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), label)
	if out, err := exec.Command(resolveTrustedBin("launchctl"), "kickstart", "-k", target).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl kickstart -k %s: %w\n%s", target, err, out)
	}
	return nil
}
