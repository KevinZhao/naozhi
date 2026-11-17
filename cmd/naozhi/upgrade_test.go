package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/selfupdate"
)

func TestUpgradeDecision(t *testing.T) {
	cases := []struct {
		name            string
		latest, current string
		force           bool
		want            upgradeAction
		wantMsg         string
	}{
		{"newer proceeds", "v0.0.83", "v0.0.82", false, upgradeProceed, "New version available: v0.0.82 → v0.0.83"},
		{"equal is already latest", "v0.0.82", "v0.0.82", false, upgradeAlreadyLatest, "Already at the latest version (v0.0.82)"},
		{"equal with force is still already latest", "v0.0.82", "v0.0.82", true, upgradeAlreadyLatest, "Already at the latest"},
		{"older refused", "v0.0.81", "v0.0.82", false, upgradeRefuse, "Latest release v0.0.81 is not newer than running v0.0.82; use --force"},
		{"older with force proceeds", "v0.0.81", "v0.0.82", true, upgradeProceed, "installing it anyway (--force)"},
		{"build past the latest tag refused", "v0.0.82", "v0.0.82-3-gabc1234", false, upgradeRefuse, "not newer than running v0.0.82-3-gabc1234"},
		{"unparseable current refused", "v0.0.82", "abc1234", false, upgradeRefuse, "not newer than running abc1234"},
		{"unparseable latest refused", "nightly", "v0.0.82", false, upgradeRefuse, "Latest release nightly is not newer"},
		{"dev refused", "v0.0.82", "dev", false, upgradeRefuse, "Running a dev build. Use --force to replace it with release v0.0.82."},
		{"dev with force proceeds", "v0.0.82", "dev", true, upgradeProceed, "dev build — upgrading to v0.0.82 (--force)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := upgradeDecision(tc.latest, tc.current, tc.force)
			if got != tc.want {
				t.Errorf("upgradeDecision(%q, %q, %v) action = %d, want %d", tc.latest, tc.current, tc.force, got, tc.want)
			}
			if !strings.Contains(msg, tc.wantMsg) || !strings.HasSuffix(msg, "\n") {
				t.Errorf("message = %q, want it to contain %q and end in a newline", msg, tc.wantMsg)
			}
		})
	}
}

// The recovery printed when a restart is not confirmed must use the service
// manager of the platform: a Mac has a LaunchAgent, no systemctl or journalctl.
func TestRestartRecovery(t *testing.T) {
	const bak, self = "/u/bin/naozhi.naozhi-upgrade.bak", "/u/bin/naozhi"
	// A new file renamed into place: cp over the binary keeps its inode, and a
	// kickstart after that left the macOS service down.
	restore := "cp " + bak + " " + self + ".restore && chmod 0755 " + self + ".restore && mv -f " + self + ".restore " + self

	check, rollback := restartRecovery("darwin", "gui/501/com.naozhi.agent", bak, self)
	if want := restore + " && launchctl kickstart -k gui/501/com.naozhi.agent"; rollback != want {
		t.Errorf("darwin rollback = %q, want %q", rollback, want)
	}
	if len(check) != 1 || check[0] != "launchctl print gui/501/com.naozhi.agent" {
		t.Errorf("darwin check = %v", check)
	}
	for _, s := range append(check, rollback) {
		if strings.Contains(s, "systemctl") || strings.Contains(s, "journalctl") || strings.Contains(s, "sudo") {
			t.Errorf("darwin recovery %q names systemd or sudo", s)
		}
	}

	// Job unconfirmed: the restore alone, no guessed kickstart.
	if check, rollback := restartRecovery("darwin", "", bak, self); len(check) != 0 || rollback != restore {
		t.Errorf("darwin without a target = %v, %q; want only %q", check, rollback, restore)
	}

	check, rollback = restartRecovery("linux", "", bak, self)
	if want := "sudo cp " + bak + " " + self + " && sudo chmod 0755 " + self + " && sudo systemctl restart naozhi"; rollback != want {
		t.Errorf("linux rollback = %q, want %q", rollback, want)
	}
	if len(check) != 2 || !strings.HasPrefix(check[0], "systemctl status") || !strings.HasPrefix(check[1], "journalctl") {
		t.Errorf("linux check = %v", check)
	}
}

// A step that ran out of its own budget says so, with the budget.
func TestStepErr(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	<-ctx.Done()
	cancel()
	err := stepErr(ctx, errors.New("copy response body: context deadline exceeded"), 30*time.Minute)
	if !strings.Contains(err.Error(), "timed out after 30m0s") {
		t.Errorf("stepErr = %v, want the budget named", err)
	}

	live, cancel := context.WithTimeout(context.Background(), time.Hour)
	cancel()
	orig := errors.New("HTTP 404")
	if got := stepErr(live, orig, time.Hour); got != orig {
		t.Errorf("stepErr on a context that did not expire = %v, want the error unchanged", got)
	}
}

// The backup goes only after a restart that confirmed the new binary is up.
// The old rule removed it whenever no restart warning was printed, so
// --no-restart and a stopped service lost the rollback before the new binary
// had ever started.
func TestUpgradeRemovesBackup(t *testing.T) {
	cases := []struct {
		name                     string
		goos                     string
		restarted, restartWarned bool
		want                     bool
	}{
		{"linux restart confirmed", "linux", true, false, true},
		{"linux restart not confirmed", "linux", true, true, false},
		{"linux no restart (--no-restart or service stopped)", "linux", false, false, false},
		// kickstart returns once launchd took the request; it confirms nothing.
		{"darwin kickstart accepted", "darwin", true, false, false},
		{"darwin no restart", "darwin", false, false, false},
	}
	for _, c := range cases {
		if got := upgradeRemovesBackup(c.goos, c.restarted, c.restartWarned); got != c.want {
			t.Errorf("%s: upgradeRemovesBackup = %v, want %v", c.name, got, c.want)
		}
	}
}

// A failed download must not leave its temp dir behind: runUpgrade exits
// through fatalf, whose os.Exit skips defers, so the cleanup has to run before
// the error reaches it. The release has no asset URL, so the download fails
// before any connection.
func TestDownloadAndReplace_FailureRemovesTempDir(t *testing.T) {
	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)

	_, err := downloadAndReplace(&selfupdate.Release{Tag: "v9.9.9"}, filepath.Join(t.TempDir(), "naozhi"))
	if err == nil || !strings.Contains(err.Error(), "download") {
		t.Fatalf("err = %v, want a download failure", err)
	}
	left, rerr := os.ReadDir(tmpRoot)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, e := range left {
		t.Errorf("left behind in $TMPDIR: %s", e.Name())
	}
}
