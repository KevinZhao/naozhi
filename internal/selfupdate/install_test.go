package selfupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

// TestInstallLatest_StagedDoesNotDownload is the RFC §1.3 regression gate, and
// the most important test in this file.
//
// In the staged state (binary on disk is already the target, process still runs
// the old one) an apply must restart ONLY. If it downloads and Replace()s again,
// the backup — which holds the one version we could roll back to — is
// overwritten with the new binary, and the deployment loses its escape hatch
// while looking perfectly healthy.
func TestInstallLatest_StagedDoesNotDownload(t *testing.T) {
	var lookups int
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		lookups++
		return &Release{Tag: "v1.1.0"}, nil
	})

	// A .bak holding the rollback version. Any Download/Replace on this path
	// would rewrite it; asserting on its bytes is stronger than asserting on a
	// call count because it is the actual thing we are protecting.
	dir := t.TempDir()
	backup := filepath.Join(dir, "naozhi"+backupSuffix)
	const rollbackBytes = "v1.0.0-binary"
	if err := os.WriteFile(backup, []byte(rollbackBytes), 0o600); err != nil {
		t.Fatal(err)
	}

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
	got, readErr := os.ReadFile(backup)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != rollbackBytes {
		t.Errorf("backup was rewritten (%q); the staged path must never Replace() again — RFC §1.3", got)
	}
	// Phase stays Staged (not Failed): the install DID succeed earlier, and
	// reporting Failed here would invite a retry, which is the destructive path.
	if snap := c.cfg.Status.Snapshot(); snap.Phase != PhaseStaged {
		t.Errorf("phase = %q, want %q after a restart that could not be performed", snap.Phase, PhaseStaged)
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
	hint := RollbackHint()
	if hint == "" {
		t.Skip("SelfPath unavailable in this environment")
	}
	if !strings.Contains(hint, backupSuffix) {
		t.Errorf("hint %q must reference the backup file %q", hint, backupSuffix)
	}
	if !strings.Contains(hint, "chmod 0755") {
		t.Errorf("hint %q must restore the exec bit; a copied backup without it will not run", hint)
	}
	// A test process is not a managed service, so no restart command may be
	// appended: launchdServiceLabel() would fall back to a constant label and
	// produce a command that fails, inside a && chain that hides the restore.
	if ServiceRunning() {
		t.Skip("this process is under a managed service; the no-service branch is what needs the assertion")
	}
	for _, unwanted := range []string{"kickstart", "systemctl"} {
		if strings.Contains(hint, unwanted) {
			t.Errorf("hint %q must not guess a restart command with no managed service detected", hint)
		}
	}
}

// With a managed service the hint DOES end in a restart — restoring the bytes
// alone leaves the bad build running.
func TestRollbackHint_IncludesRestartWhenServiceRuns(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemdUnitActive is the only service probe that can be stubbed")
	}
	orig := systemdUnitActive
	systemdUnitActive = func() bool { return true }
	t.Cleanup(func() { systemdUnitActive = orig })

	hint := RollbackHint()
	if hint == "" {
		t.Skip("SelfPath unavailable in this environment")
	}
	if !strings.Contains(hint, "systemctl restart") {
		t.Errorf("hint %q must end in a restart when a managed service exists", hint)
	}
}
