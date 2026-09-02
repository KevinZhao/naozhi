package selfupdate

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// withStubbedOnDiskVersion swaps the on-disk version probe so tests never exec
// anything. Do NOT use t.Parallel with this (package convention for the
// package's mutable test seams).
func withStubbedOnDiskVersion(t *testing.T, v string, ok bool) {
	t.Helper()
	orig := onDiskVersionFn
	onDiskVersionFn = func() (string, bool) { return v, ok }
	t.Cleanup(func() { onDiskVersionFn = orig })
}

// withNoManagedService makes the process look unmanaged to every probe, and
// installs a RestartServiceNoWait stub that records calls instead of touching
// the host's service manager. The returned func reports how many restarts were
// requested.
//
// Every InstallLatest(…, restart=true) test MUST use this. Without it, on a
// Linux host that runs naozhi under systemd, the restart-only path reaches the
// real `systemctl restart --no-block naozhi` — and the test suite restarts the
// production service it happens to be running next to.
func withNoManagedService(t *testing.T) (restarts func() int) {
	t.Helper()
	withStubbedUnitActive(t, func() bool { return false })
	withStubbedMainPID(t, 0)
	return withStubbedRestartNoWait(t, nil)
}

// withStubbedRestartNoWait swaps the RestartServiceNoWait implementation for one
// that counts calls and returns err. No-t.Parallel rule applies.
func withStubbedRestartNoWait(t *testing.T, err error) (calls func() int) {
	t.Helper()
	orig := restartServiceNoWaitFn
	n := 0
	restartServiceNoWaitFn = func(context.Context) error {
		n++
		return err
	}
	t.Cleanup(func() { restartServiceNoWaitFn = orig })
	return func() int { return n }
}

// withStubbedMainPID swaps the systemd MainPID probe. No-t.Parallel rule applies.
func withStubbedMainPID(t *testing.T, pid int) {
	t.Helper()
	orig := systemdMainPID
	systemdMainPID = func() int { return pid }
	t.Cleanup(func() { systemdMainPID = orig })
}

// TestInstallLatest_StagedDoesNotDownload is the RFC §1.3 regression gate, and
// the most important test in this file.
//
// In the staged state (binary on disk is already the target, process still runs
// the old one) an apply must restart ONLY. If it downloads and Replace()s again,
// the backup — which holds the one version we could roll back to — is
// overwritten with the new binary, and the deployment loses its escape hatch
// while looking perfectly healthy.
//
// What is asserted: the release lookup is never made (lookups == 0) — it is the
// first step of the install path, so not reaching it means no Download and no
// Replace either. An earlier version of this test also wrote a fake .bak into a
// TempDir and checked its bytes; that proved nothing, because Replace() writes
// next to SelfPath(), never into a TempDir.
func TestInstallLatest_StagedDoesNotDownload(t *testing.T) {
	var lookups int
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		lookups++
		return &Release{Tag: "v1.1.0"}, nil
	})
	restarts := withNoManagedService(t)

	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeDownload,
		Interval:       time.Hour,
		Status:         NewStatus("v1.0.0"),
	})
	// This process already installed v1.1.0 — the state the background checker
	// leaves behind under the default mode.
	c.installed = "v1.1.0"

	err := c.InstallLatest(context.Background(), true)

	// No managed service in a test environment, so the restart cannot proceed;
	// that is reported honestly rather than silently succeeding. What must NOT
	// happen is a download.
	if !errors.Is(err, ErrRestartUnsupported) {
		t.Errorf("err = %v, want ErrRestartUnsupported (no service under test)", err)
	}
	if lookups != 0 {
		t.Errorf("latestRelease called %d times; a staged binary needs no remote lookup at all", lookups)
	}
	if n := restarts(); n != 0 {
		t.Errorf("RestartServiceNoWait called %d times with no managed service; the gate must refuse before the primitive", n)
	}
	// Phase stays Staged (not Failed): the install DID succeed earlier, and
	// reporting Failed here would invite a retry, which is the destructive path.
	if snap := c.cfg.Status.Snapshot(); snap.Phase != PhaseStaged {
		t.Errorf("phase = %q, want %q after a restart that could not be performed", snap.Phase, PhaseStaged)
	}
}

// TestInstallLatest_StagedRestartsWhenManaged is the positive half: with a
// service that manages this process, the staged path calls the (stubbed)
// restart primitive exactly once, still without a lookup, and reports
// PhaseRestarting.
func TestInstallLatest_StagedRestartsWhenManaged(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the managed-process probe is stubbable on linux only")
	}
	var lookups int
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		lookups++
		return &Release{Tag: "v1.1.0"}, nil
	})
	withStubbedUnitActive(t, func() bool { return true })
	withStubbedMainPID(t, os.Getpid())
	restarts := withStubbedRestartNoWait(t, nil)

	st := NewStatus("v1.0.0")
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeDownload,
		Interval:       time.Hour,
		Status:         st,
	})
	c.installed = "v1.1.0"

	if err := c.InstallLatest(context.Background(), true); err != nil {
		t.Fatalf("InstallLatest: %v", err)
	}
	if lookups != 0 {
		t.Errorf("latestRelease called %d times on the staged path, want 0", lookups)
	}
	if n := restarts(); n != 1 {
		t.Errorf("RestartServiceNoWait called %d times, want exactly 1", n)
	}
	if snap := st.Snapshot(); snap.Phase != PhaseRestarting {
		t.Errorf("phase = %q, want %q once the restart is queued", snap.Phase, PhaseRestarting)
	}
}

// A unit that is active but runs a DIFFERENT process (an isolated instance
// beside the system service, or this very test binary) must not be restarted
// from here: it would take down the system service and leave this process on
// the old binary, parked on "restarting".
func TestInstallLatest_StagedRefusesForeignService(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the managed-process probe is stubbable on linux only")
	}
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		t.Error("staged path must not look up releases")
		return nil, errors.New("unreachable")
	})
	withStubbedUnitActive(t, func() bool { return true }) // a naozhi unit IS active…
	withStubbedMainPID(t, os.Getpid()+1)                  // …but it is not us
	restarts := withStubbedRestartNoWait(t, nil)

	st := NewStatus("v1.0.0")
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeDownload,
		Interval:       time.Hour,
		Status:         st,
	})
	c.installed = "v1.1.0"

	if err := c.InstallLatest(context.Background(), true); !errors.Is(err, ErrRestartUnsupported) {
		t.Errorf("err = %v, want ErrRestartUnsupported when the active unit runs another process", err)
	}
	if n := restarts(); n != 0 {
		t.Errorf("RestartServiceNoWait called %d times; restarting a service that does not run us is the F2 bug", n)
	}
	if snap := st.Snapshot(); snap.Phase != PhaseStaged {
		t.Errorf("phase = %q, want %q", snap.Phase, PhaseStaged)
	}
}

// TestInstallLatest_OnDiskProbeShortCircuits covers the gap `installed` cannot
// see: a `naozhi upgrade` run from a shell stages the binary in a DIFFERENT
// process, so this one's `installed` is empty while the bytes are already there.
// Without the probe the apply would Replace() a second time and destroy the
// backup exactly as in the test above.
func TestInstallLatest_OnDiskProbeShortCircuits(t *testing.T) {
	var lookups int
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		lookups++
		return &Release{Tag: "v1.1.0"}, nil
	})
	withStubbedOnDiskVersion(t, "v1.1.0", true) // someone else already staged it
	restarts := withNoManagedService(t)

	st := NewStatus("v1.0.0")
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeDownload,
		Interval:       time.Hour,
		Status:         st,
	})
	// installed deliberately empty: this process has not installed anything.

	err := c.InstallLatest(context.Background(), true)
	if !errors.Is(err, ErrRestartUnsupported) {
		t.Errorf("err = %v, want ErrRestartUnsupported (restart-only path, no service under test)", err)
	}
	if lookups != 1 {
		t.Errorf("latestRelease called %d times, want 1", lookups)
	}
	if n := restarts(); n != 0 {
		t.Errorf("RestartServiceNoWait called %d times with no managed service, want 0", n)
	}
	if c.installed != "v1.1.0" {
		t.Errorf("installed = %q; the probe's finding must be recorded so later ticks short-circuit too", c.installed)
	}
	snap := st.Snapshot()
	if snap.Staged != "v1.1.0" {
		t.Errorf("staged = %q, want v1.1.0 — the dashboard has to report a restart, not an install", snap.Staged)
	}
}

// A probe that cannot answer must not block an install: it is a safety net, not
// a gate. Here it reports a DIFFERENT version, so the normal install path runs
// (and fails on the download stub, which is all we assert).
func TestInstallLatest_ProbeMismatchProceeds(t *testing.T) {
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		return &Release{Tag: "v1.1.0"}, nil
	})
	withStubbedOnDiskVersion(t, "v1.0.0", true) // disk still holds the old one

	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeDownload,
		Interval:       time.Hour,
		Status:         NewStatus("v1.0.0"),
	})
	// Reaching doInstall means Download runs. The stubbed Release carries no
	// AssetURL, so fetchFile rejects it before opening any connection — the
	// assertion is that we GOT to the download step, not that a download works.
	err := c.InstallLatest(context.Background(), false)
	if err == nil {
		t.Fatal("expected the install attempt to fail on the empty asset URL, got nil")
	}
	if !strings.Contains(err.Error(), "download") {
		t.Errorf("err = %v; want a download failure, i.e. the install path was actually entered", err)
	}
	if errors.Is(err, ErrNothingToDo) || errors.Is(err, ErrRestartUnsupported) {
		t.Errorf("err = %v; a stale disk version must not short-circuit the install", err)
	}
	if snap := c.cfg.Status.Snapshot(); snap.Phase != PhaseFailed {
		t.Errorf("phase = %q, want %q", snap.Phase, PhaseFailed)
	}
}

// TestInstallLatest_SingleFlight: a second apply arriving while one is running
// must be refused immediately, not queued. A queued install would run in the
// staged state the first one just created — the backup-destroying repeat.
func TestInstallLatest_SingleFlight(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		close(entered)
		<-release // hold the first caller inside the critical section
		return nil, errors.New("stop here")
	})

	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeDownload,
		Interval:       time.Hour,
		Status:         NewStatus("v1.0.0"),
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = c.InstallLatest(context.Background(), false)
	}()
	<-entered

	if err := c.InstallLatest(context.Background(), false); !errors.Is(err, ErrInstallInProgress) {
		t.Errorf("second concurrent apply err = %v, want ErrInstallInProgress", err)
	}
	close(release)
	wg.Wait()
}

// The background tick and an apply must not both install. installLocked holds
// the same mutex, so a tick arriving mid-apply finds it taken; here we assert
// the reverse direction is serialized rather than concurrent.
func TestInstallLatest_MutexSharedWithTick(t *testing.T) {
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeDownload,
		Interval:       time.Hour,
		Status:         NewStatus("v1.0.0"),
	})
	c.installMu.Lock() // simulate a tick installing right now
	defer c.installMu.Unlock()

	withStubbedLatest(t, func(context.Context) (*Release, error) {
		t.Error("apply must not reach the network while a tick holds installMu")
		return nil, errors.New("unreachable")
	})
	if err := c.InstallLatest(context.Background(), false); !errors.Is(err, ErrInstallInProgress) {
		t.Errorf("err = %v, want ErrInstallInProgress while the tick holds installMu", err)
	}
}

// installLocked re-checks the already-installed short-circuit inside the lock,
// so a tick that raced past checkOnce's optimistic check still does not install
// twice.
func TestInstallLocked_ShortCircuitsOnInstalledTag(t *testing.T) {
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeDownload,
		Interval:       time.Hour,
		Status:         NewStatus("v1.0.0"),
	})
	c.installed = "v1.1.0"
	if err := c.installLocked(context.Background(), &Release{Tag: "v1.1.0"}, false); !errors.Is(err, ErrNothingToDo) {
		t.Errorf("err = %v, want ErrNothingToDo for a tag already installed", err)
	}
}

// Up-to-date deployment: nothing to apply, and no attempt to install.
func TestInstallLatest_NothingToDo(t *testing.T) {
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		return &Release{Tag: "v1.0.0"}, nil
	})
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeDownload,
		Interval:       time.Hour,
		Status:         NewStatus("v1.0.0"),
	})
	if err := c.InstallLatest(context.Background(), true); !errors.Is(err, ErrNothingToDo) {
		t.Errorf("err = %v, want ErrNothingToDo", err)
	}
}

// A remote tag OLDER than the running version must never be installed
// (R20260602141221-SEC-1) — the apply path is subject to the same
// no-downgrade rule as the background tick.
func TestInstallLatest_RefusesDowngrade(t *testing.T) {
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		return &Release{Tag: "v0.9.0"}, nil
	})
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeDownload,
		Interval:       time.Hour,
		Status:         NewStatus("v1.0.0"),
	})
	if err := c.InstallLatest(context.Background(), true); !errors.Is(err, ErrNothingToDo) {
		t.Errorf("err = %v, want ErrNothingToDo for an older remote tag", err)
	}
}

// dev builds never reach the network, matching checkOnce/CheckNow.
func TestInstallLatest_DevBuildSkips(t *testing.T) {
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		t.Error("dev build must not query for releases")
		return nil, errors.New("unreachable")
	})
	c := NewChecker(CheckerConfig{
		CurrentVersion: "dev",
		Mode:           ModeDownload,
		Interval:       time.Hour,
		Status:         NewStatus("dev"),
	})
	if err := c.InstallLatest(context.Background(), true); !errors.Is(err, ErrCheckSkippedDev) {
		t.Errorf("err = %v, want ErrCheckSkippedDev", err)
	}
}

// A failed release lookup is not an install failure: nothing was touched, so
// the phase must not become Failed (which would tell the operator a retry is
// needed for an install that never started) — only CheckErr moves.
func TestInstallLatest_LookupFailureLeavesPhase(t *testing.T) {
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		return nil, errors.New("network down")
	})
	st := NewStatus("v1.0.0")
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeDownload,
		Interval:       time.Hour,
		Status:         st,
	})
	if err := c.InstallLatest(context.Background(), true); err == nil {
		t.Fatal("expected the lookup error to be returned")
	}
	snap := st.Snapshot()
	if snap.Phase == PhaseFailed {
		t.Error("a failed lookup must not report PhaseFailed; nothing was installed")
	}
	if snap.CheckErr == "" {
		t.Error("the lookup failure should be recorded in CheckErr")
	}
}

// A nil Checker (auto-update disabled) is safe to call.
func TestInstallLatest_NilChecker(t *testing.T) {
	var c *Checker
	if err := c.InstallLatest(context.Background(), true); err == nil {
		t.Error("nil Checker must return an error, not panic or silently succeed")
	}
}

// RollbackHint must name the backup file and end in a restart, and must be
// paste-ready — it is the operator's only recourse when a new build will not
// boot and the dashboard is gone with it.
func TestRollbackHint(t *testing.T) {
	// serviceRunning=false is now an argument rather than an environment fact,
	// so the no-service branch is reachable on any host — it used to be skipped
	// on a machine that happened to run naozhi under launchd/systemd.
	hint := RollbackHint(false)
	if hint == "" {
		t.Skip("SelfPath unavailable in this environment")
	}
	if !strings.Contains(hint, backupSuffix) {
		t.Errorf("hint %q must reference the backup file %q", hint, backupSuffix)
	}
	if !strings.Contains(hint, "chmod 0755") {
		t.Errorf("hint %q must restore the exec bit; a copied backup without it will not run", hint)
	}
	// With no managed service no restart command may be appended:
	// launchdServiceLabel() would fall back to a constant label and produce a
	// command that fails, inside a && chain that hides the restore.
	for _, unwanted := range []string{"kickstart", "systemctl"} {
		if strings.Contains(hint, unwanted) {
			t.Errorf("hint %q must not guess a restart command with no managed service detected", hint)
		}
	}
}

// With a managed service the hint DOES end in a restart — restoring the bytes
// alone leaves the bad build running. Both platforms are covered now that the
// service verdict is injected: this case used to be linux-only because
// systemdUnitActive was the only probe that could be stubbed.
func TestRollbackHint_IncludesRestartWhenServiceRuns(t *testing.T) {
	hint := RollbackHint(true)
	if hint == "" {
		t.Skip("SelfPath unavailable in this environment")
	}
	want := "systemctl restart"
	if runtime.GOOS == "darwin" {
		want = "launchctl kickstart -k"
	}
	if !strings.Contains(hint, want) {
		t.Errorf("hint %q must end in %q when a managed service exists", hint, want)
	}
}

// ServiceManagesThisProcess is the gate every in-process restart goes through.
// "A unit is active" is not enough: the unit must run US. Linux-only because
// the darwin branch shells out to launchctl with nothing to stub.
func TestServiceManagesThisProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd probes are stubbable on linux only")
	}
	cases := []struct {
		name   string
		active bool
		pid    int
		want   bool
	}{
		{"active unit, we are MainPID", true, os.Getpid(), true},
		{"active unit, MainPID is another process", true, os.Getpid() + 1, false},
		{"active unit, MainPID unknown", true, 0, false},
		{"inactive unit even if pid matched", false, os.Getpid(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withStubbedUnitActive(t, func() bool { return tc.active })
			withStubbedMainPID(t, tc.pid)
			if got := ServiceManagesThisProcess(); got != tc.want {
				t.Errorf("ServiceManagesThisProcess() = %v, want %v", got, tc.want)
			}
		})
	}
}

// ServiceRunning keeps the "is there a daemon at all" meaning the CLI upgrader
// relies on: `naozhi upgrade` in a shell is never MainPID, and it must still
// find the service to restart.
func TestServiceRunning_DoesNotRequireSelfPID(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd probes are stubbable on linux only")
	}
	withStubbedUnitActive(t, func() bool { return true })
	withStubbedMainPID(t, os.Getpid()+1)
	if !ServiceRunning() {
		t.Error("ServiceRunning() must be true for an active unit regardless of which process asks")
	}
}

// The self-restart primitive must gate on ServiceManagesThisProcess, never on
// ServiceRunning. This is a source guard because the behaviour it protects —
// `systemctl restart naozhi` reaching a service that does not run this process
// — is exactly what a unit test must never be allowed to exercise for real.
func TestRestartSystemdNoWait_GatesOnSelfManaged(t *testing.T) {
	src, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	body := funcBody(t, string(src), "func restartSystemdNoWait()")
	if !strings.Contains(body, "if !ServiceManagesThisProcess()") {
		t.Error("restartSystemdNoWait must gate on !ServiceManagesThisProcess(); an active unit that runs another process is not ours to restart")
	}
	if strings.Contains(body, "ServiceRunning()") {
		t.Error("restartSystemdNoWait must not consult ServiceRunning(): it answers a different question (see F2)")
	}
}

// ManualCommand is what the browser prints when it cannot apply; it must come
// from the server's platform and label, never from navigator.platform.
func TestManualCommand(t *testing.T) {
	if got := ManualCommand(ActionInstall, false); got != "naozhi upgrade" {
		t.Errorf("install, unmanaged: %q, want naozhi upgrade", got)
	}
	if got := ManualCommand(ActionInstall, true); got != "naozhi upgrade" {
		t.Errorf("install, managed: %q, want naozhi upgrade", got)
	}
	// Restart without a service that manages this process: no command. The
	// preflight reason already says "restart the process by hand", and
	// `systemctl restart naozhi` here would restart something else.
	if got := ManualCommand(ActionRestart, false); got != "" {
		t.Errorf("restart, unmanaged: %q, want empty", got)
	}
	if got := ManualCommand(ActionNone, true); got != "" {
		t.Errorf("none: %q, want empty", got)
	}

	got := ManualCommand(ActionRestart, true)
	switch runtime.GOOS {
	case "darwin":
		t.Setenv(xpcServiceNameEnv, "com.naozhi.agent")
		got = ManualCommand(ActionRestart, true)
		if !strings.HasPrefix(got, "launchctl kickstart -k gui/") || !strings.HasSuffix(got, "/com.naozhi.agent") {
			t.Errorf("restart on darwin = %q; want kickstart -k gui/<uid>/<real label>", got)
		}
	default:
		if got != "sudo systemctl restart naozhi" {
			t.Errorf("restart on %s = %q, want sudo systemctl restart naozhi", runtime.GOOS, got)
		}
	}
	// The same command RollbackHint appends, so the two never disagree.
	if hint := RollbackHint(true); hint != "" && !strings.HasSuffix(hint, " && "+got) {
		t.Errorf("RollbackHint(true) = %q must end with ManualCommand(restart) = %q", hint, got)
	}
}
