package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// withStubbedLatest swaps latestRelease for a test and restores it after.
// Do NOT use t.Parallel with this (package convention).
func withStubbedLatest(t *testing.T, fn func(context.Context) (*Release, error)) {
	t.Helper()
	orig := latestRelease
	latestRelease = fn
	t.Cleanup(func() { latestRelease = orig })
}

func TestParseMode(t *testing.T) {
	cases := map[string]Mode{
		"notify":   ModeNotify,
		"download": ModeDownload,
		"auto":     ModeAuto,
		"":         ModeDownload, // unknown → safe configured default
		"garbage":  ModeDownload,
	}
	for in, want := range cases {
		if got := ParseMode(in); got != want {
			t.Errorf("ParseMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewChecker_NilOnBadInterval(t *testing.T) {
	if c := NewChecker(CheckerConfig{Interval: 0}); c != nil {
		t.Error("NewChecker with zero interval should return nil")
	}
	if c := NewChecker(CheckerConfig{Interval: -time.Hour}); c != nil {
		t.Error("NewChecker with negative interval should return nil")
	}
	if c := NewChecker(CheckerConfig{Interval: time.Hour}); c == nil {
		t.Error("NewChecker with valid interval should not be nil")
	}
}

func TestNewChecker_DefaultsModeToDownload(t *testing.T) {
	c := NewChecker(CheckerConfig{Interval: time.Hour})
	if c.cfg.Mode != ModeDownload {
		t.Errorf("default mode = %q, want %q", c.cfg.Mode, ModeDownload)
	}
}

// R20260602141221-SEC-1: table-driven semver comparison tests.
func TestSemverGreater(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.0.2", "v0.0.1", true},
		{"v0.1.0", "v0.0.9", true},
		{"v1.0.0", "v0.9.9", true},
		{"v0.0.1", "v0.0.2", false},
		{"v0.0.1", "v0.0.1", false}, // equal → not greater
		{"v1.0.0", "v1.0.0", false},
		// pre-release suffix ignored
		{"v0.0.3", "v0.0.2-rc.1", true},
		{"v0.0.2-rc.1", "v0.0.1", true},
		// malformed → conservative false
		{"invalid", "v0.0.1", false},
		{"v0.0.1", "invalid", false},
		{"", "v0.0.1", false},
		{"v0.0.1", "", false},
	}
	for _, tc := range cases {
		got := semverGreater(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("semverGreater(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// IsNewer is the exported form of semverGreater that `naozhi upgrade` gates on.
func TestIsNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.0.83", "v0.0.82", true},
		{"v0.0.82", "v0.0.82", false},
		{"v0.0.81", "v0.0.82", false},
		{"v0.0.82", "v0.0.82-3-gabc1234", false},
		{"v0.0.82", "dev", false},
		{"dev", "v0.0.82", false},
	}
	for _, tc := range cases {
		if got := IsNewer(tc.a, tc.b); got != tc.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// Downgrade attempt: remote reports an older tag → no action.
func TestCheckOnce_NoDowngrade(t *testing.T) {
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		return &Release{Tag: "v0.0.1"}, nil // older than current
	})
	var notices int
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeNotify,
		Interval:       time.Hour,
		Notify:         func(string) { notices++ },
	})
	c.checkOnce(context.Background())
	if notices != 0 {
		t.Errorf("downgrade attempt should emit no notice, got %d", notices)
	}
}

// notify mode: a newer release fires exactly one notice and does NOT touch
// the binary (no download stub needed — doInstall is never reached).
func TestCheckOnce_NotifyMode(t *testing.T) {
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		return &Release{Tag: "v9.9.9"}, nil
	})
	var got []string
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeNotify,
		Interval:       time.Hour,
		Notify:         func(s string) { got = append(got, s) },
	})
	c.checkOnce(context.Background())
	if len(got) != 1 {
		t.Fatalf("expected 1 notice, got %d: %v", len(got), got)
	}
}

// Up-to-date: no notice, no work.
func TestCheckOnce_UpToDate(t *testing.T) {
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		return &Release{Tag: "v1.0.0"}, nil
	})
	var notices int
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeNotify,
		Interval:       time.Hour,
		Notify:         func(string) { notices++ },
	})
	c.checkOnce(context.Background())
	if notices != 0 {
		t.Errorf("up-to-date should emit no notice, got %d", notices)
	}
}

// dev build self-skips before any network call.
func TestCheckOnce_DevBuildSkips(t *testing.T) {
	called := false
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		called = true
		return &Release{Tag: "v9.9.9"}, nil
	})
	c := NewChecker(CheckerConfig{
		CurrentVersion: "dev",
		Mode:           ModeNotify,
		Interval:       time.Hour,
		Notify:         func(string) {},
	})
	c.checkOnce(context.Background())
	if called {
		t.Error("dev build must not query for releases")
	}
}

// A failed release lookup logs+swallows: no notice, no panic.
func TestCheckOnce_LookupFailureSwallowed(t *testing.T) {
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		return nil, errors.New("network down")
	})
	var notices int
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeNotify,
		Interval:       time.Hour,
		Notify:         func(string) { notices++ },
	})
	c.checkOnce(context.Background()) // must not panic
	if notices != 0 {
		t.Errorf("lookup failure should emit no notice, got %d", notices)
	}
}

// Run honors ctx cancellation promptly and stops the loop.
func TestRun_StopsOnContextCancel(t *testing.T) {
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		return &Release{Tag: "v1.0.0"}, nil
	})
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeNotify,
		Interval:       time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly after ctx cancel")
	}
}

// nil Checker.Run is a safe no-op (NewChecker returned nil path).
func TestRun_NilChecker(t *testing.T) {
	var c *Checker
	done := make(chan struct{})
	go func() { c.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("nil Checker.Run should return immediately")
	}
}

// notify with a nil NotifyFunc must not panic.
func TestNotify_NilFuncSafe(t *testing.T) {
	c := NewChecker(CheckerConfig{Interval: time.Hour})
	c.notify("anything") // no panic, no-op
}

// concurrent notice delivery from the closure is data-race safe under -race
// when the caller's NotifyFunc guards its own state.
func TestNotify_Concurrent(t *testing.T) {
	var mu sync.Mutex
	var n int
	c := NewChecker(CheckerConfig{
		Interval: time.Hour,
		Notify: func(string) {
			mu.Lock()
			n++
			mu.Unlock()
		},
	})
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.notify("x") }()
	}
	wg.Wait()
	if n != 10 {
		t.Errorf("expected 10 notices, got %d", n)
	}
}

// The periodic check in the staged state must not install a still newer
// release: Replace would back the never-run staged binary up over the backup
// holding the running version, the only one known to boot (RFC §1.3, G7).
// Reaching doInstall shows as PhaseFailed and a failure notice — the stubbed
// release has no asset URL, so its download fails before any connection.
func TestCheckOnce_StagedDoesNotReplaceAgain(t *testing.T) {
	for _, mode := range []Mode{ModeDownload, ModeAuto} {
		t.Run(string(mode), func(t *testing.T) {
			withStubbedLatest(t, func(context.Context) (*Release, error) {
				return &Release{Tag: "v1.2.0"}, nil
			})
			withStubbedOnDiskVersion(t, "v1.1.0", true)
			restarts := withNoManagedService(t)
			var notices []string
			st := NewStatus("v1.0.0")
			c := NewChecker(CheckerConfig{
				CurrentVersion: "v1.0.0",
				Mode:           mode,
				Interval:       time.Hour,
				Status:         st,
				Notify:         func(s string) { notices = append(notices, s) },
			})
			// An earlier tick of this process installed v1.1.0; nobody restarted.
			c.installed = "v1.1.0"
			st.notePhase(PhaseStaged, "v1.1.0", nil)

			_ = c.checkOnce(context.Background())

			snap := st.Snapshot()
			if snap.Phase != PhaseStaged || snap.Staged != "v1.1.0" {
				t.Errorf("phase = %q staged = %q, want %q v1.1.0: the tick went on to install v1.2.0 over the staged binary", snap.Phase, snap.Staged, PhaseStaged)
			}
			if len(notices) != 0 {
				t.Errorf("notices = %v, want none: the tick tried to install over the staged binary", notices)
			}
			if n := restarts(); n != 0 {
				t.Errorf("RestartServiceNoWait called %d times, want 0", n)
			}
			if c.installed != "v1.1.0" {
				t.Errorf("installed = %q, want v1.1.0", c.installed)
			}
		})
	}
}

// `naozhi upgrade` from a shell stages the binary without this process knowing
// (`installed` stays empty); the on-disk probe has to stop the tick the same
// way, and the staged version is recorded so the dashboard offers a restart.
func TestCheckOnce_ShellStagedOnDiskDoesNotReplace(t *testing.T) {
	for _, latest := range []string{"v1.1.0", "v1.2.0"} {
		t.Run(latest, func(t *testing.T) {
			withStubbedLatest(t, func(context.Context) (*Release, error) {
				return &Release{Tag: latest}, nil
			})
			withStubbedOnDiskVersion(t, "v1.1.0", true)
			var notices []string
			st := NewStatus("v1.0.0")
			c := NewChecker(CheckerConfig{
				CurrentVersion: "v1.0.0",
				Mode:           ModeDownload,
				Interval:       time.Hour,
				Status:         st,
				Notify:         func(s string) { notices = append(notices, s) },
			})

			_ = c.checkOnce(context.Background())

			if snap := st.Snapshot(); snap.Phase != PhaseStaged || snap.Staged != "v1.1.0" {
				t.Errorf("phase = %q staged = %q, want %q v1.1.0", snap.Phase, snap.Staged, PhaseStaged)
			}
			if len(notices) != 0 {
				t.Errorf("notices = %v, want none: the tick tried to install over the binary staged on disk", notices)
			}
			if c.installed != "v1.1.0" {
				t.Errorf("installed = %q, want v1.1.0 so later ticks stop before the probe", c.installed)
			}
		})
	}
}

// An older binary on disk is not staged (a restart would downgrade), and the
// tick still does not replace it.
func TestCheckOnce_OlderOnDiskNotRecordedAsStaged(t *testing.T) {
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		return &Release{Tag: "v1.2.0"}, nil
	})
	withStubbedOnDiskVersion(t, "v0.9.0", true)
	st := NewStatus("v1.0.0")
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeDownload,
		Interval:       time.Hour,
		Status:         st,
	})
	_ = c.checkOnce(context.Background())
	if snap := st.Snapshot(); snap.Staged != "" || snap.Phase == PhaseFailed {
		t.Errorf("phase = %q staged = %q, want nothing staged and no install attempt", snap.Phase, snap.Staged)
	}
	if c.installed != "" {
		t.Errorf("installed = %q, want empty", c.installed)
	}
}

// With the on-disk binary still the running version the tick installs as
// before (and here fails at the download, which is all that is asserted).
func TestCheckOnce_SameOnDiskInstalls(t *testing.T) {
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		return &Release{Tag: "v1.2.0"}, nil
	})
	withStubbedOnDiskVersion(t, "v1.0.0", true)
	st := NewStatus("v1.0.0")
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeDownload,
		Interval:       time.Hour,
		Status:         st,
	})
	_ = c.checkOnce(context.Background())
	if snap := st.Snapshot(); snap.Phase != PhaseFailed {
		t.Errorf("phase = %q, want %q: the install path was not entered", snap.Phase, PhaseFailed)
	}
}

// withManagedService makes ServiceManagesThisProcess true on linux (unit
// active, MainPID ours) and darwin (a verified launchd job running this test
// binary), and stubs the restart primitive so nothing on the host restarts.
func withManagedService(t *testing.T) (restarts func() int) {
	t.Helper()
	switch runtime.GOOS {
	case "linux":
		withStubbedUnitActive(t, func() bool { return true })
		withStubbedMainPID(t, os.Getpid())
	case "darwin":
		self, err := SelfPath()
		if err != nil {
			t.Skipf("SelfPath unavailable: %v", err)
		}
		const label = "com.naozhi.agent"
		t.Setenv(xpcServiceNameEnv, label)
		withFakeExec(t, &fakeExec{t: t, listOut: fmt.Sprintf("{\n\t\"Label\" = %q;\n\t\"PID\" = 1552;\n\t\"Program\" = %q;\n};", label, self)})
	default:
		t.Skipf("no managed service on %s", runtime.GOOS)
	}
	if !ServiceManagesThisProcess() {
		t.Fatal("stubs did not make the process look managed")
	}
	return withStubbedRestartNoWait(t, nil)
}

// update.mode auto restarts on its own: a binary staged from a shell
// (`naozhi upgrade --no-restart`, or one that found the service "not running")
// must be restarted into by the tick, not left until someone restarts by hand.
// Only the restart is triggered, never a second Replace.
func TestCheckOnce_AutoRestartsIntoShellStagedBinary(t *testing.T) {
	for _, latest := range []string{"v1.1.0", "v1.2.0"} {
		t.Run(latest, func(t *testing.T) {
			withStubbedLatest(t, func(context.Context) (*Release, error) {
				return &Release{Tag: latest}, nil
			})
			withStubbedOnDiskVersion(t, "v1.1.0", true)
			restarts := withManagedService(t)
			var notices []string
			st := NewStatus("v1.0.0")
			c := NewChecker(CheckerConfig{
				CurrentVersion: "v1.0.0",
				Mode:           ModeAuto,
				Interval:       time.Hour,
				Status:         st,
				Notify:         func(s string) { notices = append(notices, s) },
			})

			_ = c.checkOnce(context.Background())

			if n := restarts(); n != 1 {
				t.Errorf("RestartServiceNoWait called %d times, want 1: auto mode left the shell-staged v1.1.0 waiting for a manual restart", n)
			}
			if snap := st.Snapshot(); snap.Phase != PhaseRestarting || snap.Staged != "v1.1.0" {
				t.Errorf("phase = %q staged = %q, want %q v1.1.0", snap.Phase, snap.Staged, PhaseRestarting)
			}
			for _, n := range notices {
				if strings.Contains(n, "失败") {
					t.Errorf("notice %q: the tick went on to install instead of restarting", n)
				}
			}
		})
	}
}

// An auto install in a process no service manages must not first announce a
// restart and then, in the next notice, that the restart failed: one notice,
// the manual-restart one, with Staged + ErrRestartUnsupported for the UI.
func TestRestartAfterInstall_UnmanagedSendsOnlyManualNotice(t *testing.T) {
	switch runtime.GOOS {
	case "linux":
	case "darwin":
		t.Setenv(xpcServiceNameEnv, "com.naozhi.agent")
		withFakeExec(t, &fakeExec{t: t, listFail: true})
	default:
		t.Skipf("no managed service on %s", runtime.GOOS)
	}
	restarts := withNoManagedService(t)
	var notices []string
	st := NewStatus("v1.0.0")
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeAuto,
		Interval:       time.Hour,
		Status:         st,
		Notify:         func(s string) { notices = append(notices, s) },
	})

	err := c.restartAfterInstall(context.Background(), "v1.2.0", "/x.bak")

	if !errors.Is(err, ErrRestartUnsupported) {
		t.Errorf("err = %v, want ErrRestartUnsupported", err)
	}
	if n := restarts(); n != 0 {
		t.Errorf("RestartServiceNoWait called %d times with no managed service", n)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "重启触发失败") {
		t.Errorf("notices = %q, want only the manual-restart notice", notices)
	}
	if snap := st.Snapshot(); snap.Phase != PhaseStaged || snap.Staged != "v1.2.0" || !strings.Contains(snap.LastErr, ErrRestartUnsupported.Error()) {
		t.Errorf("phase = %q staged = %q lastErr = %q, want Staged v1.2.0 with ErrRestartUnsupported", snap.Phase, snap.Staged, snap.LastErr)
	}
}

// With a managed service the restart is announced and triggered once.
func TestRestartAfterInstall_ManagedAnnouncesAndRestarts(t *testing.T) {
	restarts := withManagedService(t)
	var notices []string
	st := NewStatus("v1.0.0")
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Mode:           ModeAuto,
		Interval:       time.Hour,
		Status:         st,
		Notify:         func(s string) { notices = append(notices, s) },
	})

	if err := c.restartAfterInstall(context.Background(), "v1.2.0", "/x.bak"); err != nil {
		t.Fatalf("restartAfterInstall: %v", err)
	}
	if n := restarts(); n != 1 {
		t.Errorf("RestartServiceNoWait called %d times, want 1", n)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "并重启") {
		t.Errorf("notices = %q, want only the restarting notice", notices)
	}
	if snap := st.Snapshot(); snap.Phase != PhaseRestarting {
		t.Errorf("phase = %q, want %q", snap.Phase, PhaseRestarting)
	}
}
