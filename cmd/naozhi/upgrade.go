package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/naozhi/naozhi/internal/selfupdate"
)

func runUpgrade(args []string) {
	fs := newFlagSet("upgrade")
	checkOnly := fs.Bool("check-only", false, "check for a newer version without downloading")
	noRestart := fs.Bool("no-restart", false, "skip service restart after upgrade")
	force := fs.Bool("force", false, "install the latest release even if it is not newer than the running build (dev builds included)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: naozhi upgrade [flags]

Upgrade naozhi to the latest release from GitHub.

The binary is downloaded, SHA-256 verified, and atomically swapped into place.
If a system service (systemd/launchd) is running it is restarted automatically.

Flags:
`)
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	// Each step has its own budget, so a slow download cannot eat the
	// restart's and a timeout names the step that ran out. stepErr may read a
	// context after its cancel: an expired deadline keeps DeadlineExceeded.

	// 1. Resolve latest release tag.
	fmt.Printf("Checking latest release…\n")
	ctx, cancel := context.WithTimeout(context.Background(), upgradeCheckTimeout)
	rel, err := selfupdate.LatestRelease(ctx)
	cancel()
	if err != nil {
		fatalf("check latest release: %v\n", stepErr(ctx, err, upgradeCheckTimeout))
	}

	// 2. Compare with running version.
	action, msg := upgradeDecision(rel.Tag, version, *force)
	switch action {
	case upgradeAlreadyLatest:
		fmt.Print(msg)
		return
	case upgradeRefuse:
		fmt.Fprint(os.Stderr, msg)
		os.Exit(1)
	}
	fmt.Print(msg)

	if *checkOnly {
		return
	}

	// 3. Locate the running binary.
	selfPath, err := selfupdate.SelfPath()
	if err != nil {
		fatalf("locate running binary: %v\n", err)
	}

	// 4. Download into a temp dir.
	tmp, err := os.MkdirTemp("", "naozhi-upgrade-*")
	if err != nil {
		fatalf("create temp dir: %v\n", err)
	}
	defer os.RemoveAll(tmp)

	fmt.Printf("Downloading %s…\n", rel.Tag)
	dctx, dcancel := context.WithTimeout(context.Background(), selfupdate.DownloadTimeout)
	newBin, err := selfupdate.Download(dctx, rel, tmp)
	dcancel()
	if err != nil {
		fatalf("download: %v\n", stepErr(dctx, err, selfupdate.DownloadTimeout))
	}
	fmt.Printf("Checksum verified.\n")

	// 5. Replace binary (atomic: stage → rename).
	fmt.Printf("Installing to %s…\n", selfPath)
	backupPath, err := selfupdate.Replace(newBin, selfPath)
	if err != nil {
		fatalf("replace binary: %v\n", err)
	}

	// 6. Restart service (unless skipped or not running). Deliberately NO
	// rollback on restart error: the binary is already verified, and a restart
	// failure is almost always a slow Type=notify cold start rather than a bad
	// binary; Restart=always keeps bringing it up.
	serviceWasRunning := selfupdate.ServiceRunning()
	restartWarned := false
	if !*noRestart && serviceWasRunning {
		fmt.Printf("Restarting service…\n")
		rctx, rcancel := context.WithTimeout(context.Background(), upgradeRestartTimeout)
		err := selfupdate.RestartService(rctx)
		rcancel()
		if err != nil {
			restartWarned = true
			fmt.Fprintf(os.Stderr, "\nwarning: service restart could not be confirmed: %v\n", stepErr(rctx, err, upgradeRestartTimeout))
			fmt.Fprintf(os.Stderr, "The new binary IS installed and verified. naozhi may still be starting\n")
			fmt.Fprintf(os.Stderr, "(slow cold start) OR may be failing to start. Check which:\n")
			check, rollback := restartRecovery(runtime.GOOS, selfupdate.LaunchdServiceTarget(), backupPath, selfPath)
			for _, c := range check {
				fmt.Fprintf(os.Stderr, "    %s\n", c)
			}
			fmt.Fprintf(os.Stderr, "To roll back to the previous binary:\n")
			fmt.Fprintf(os.Stderr, "    %s\n", rollback)
		}
	} else if !serviceWasRunning {
		fmt.Printf("Service not running — skipping restart.\n")
	}

	// 7. Keep the backup when the restart was not confirmed (manual rollback).
	if !restartWarned {
		_ = os.Remove(backupPath)
	}

	fmt.Printf("\n✓ naozhi upgraded to %s\n", rel.Tag)

	if *noRestart || !serviceWasRunning {
		fmt.Printf("  Restart the service manually to apply the new binary.\n")
	}
}

const (
	// upgradeCheckTimeout bounds the release lookup (one redirect).
	upgradeCheckTimeout = 60 * time.Second
	// upgradeRestartTimeout covers systemd's wait for the unit to come back
	// active (3 minutes) with room to spare; launchd's kickstart does not wait.
	upgradeRestartTimeout = 4 * time.Minute
)

// stepErr says which budget ran out when a step's own context expired, since
// "context deadline exceeded" alone does not tell a slow download from a hung
// restart.
func stepErr(ctx context.Context, err error, budget time.Duration) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("timed out after %s: %w", budget, err)
	}
	return err
}

// restartRecovery returns the commands to check on the service and the one
// that restores the previous binary when the restart was not confirmed. The
// backup is 0600 non-executable, so the restore MUST chmod. On macOS the
// service is a LaunchAgent of this user: launchctl, no sudo, and the restore
// renames a new file into place (selfupdate.RestoreCommand); launchdTarget is
// "" when the job could not be confirmed, then only the restore is printed
// (a guessed kickstart would fail inside the && chain).
func restartRecovery(goos, launchdTarget, backupPath, selfPath string) (check []string, rollback string) {
	if goos == "darwin" {
		rollback = selfupdate.RestoreCommand(backupPath, selfPath)
		if launchdTarget == "" {
			return nil, rollback
		}
		return []string{"launchctl print " + launchdTarget},
			rollback + " && launchctl kickstart -k " + launchdTarget
	}
	return []string{"systemctl status naozhi", "journalctl -u naozhi -n 50 --no-pager"},
		fmt.Sprintf("sudo cp %s %s && sudo chmod 0755 %s && sudo systemctl restart naozhi", backupPath, selfPath, selfPath)
}

type upgradeAction int

const (
	upgradeProceed upgradeAction = iota
	upgradeAlreadyLatest
	upgradeRefuse
)

// upgradeDecision decides whether to install latest over the running build
// and returns the line to print. Only a strictly newer release proceeds on its
// own, the same rule the background checker applies, so a release marked
// "latest" that is older than the running build is not a silent downgrade.
func upgradeDecision(latest, current string, force bool) (upgradeAction, string) {
	switch {
	case latest == current:
		return upgradeAlreadyLatest, fmt.Sprintf("Already at the latest version (%s).\n", current)
	case current == "dev" && force:
		return upgradeProceed, fmt.Sprintf("dev build — upgrading to %s (--force)\n", latest)
	case current == "dev":
		return upgradeRefuse, fmt.Sprintf("Running a dev build. Use --force to replace it with release %s.\n", latest)
	case selfupdate.IsNewer(latest, current):
		return upgradeProceed, fmt.Sprintf("New version available: %s → %s\n", current, latest)
	case force:
		return upgradeProceed, fmt.Sprintf("Latest release %s is not newer than running %s — installing it anyway (--force)\n", latest, current)
	default:
		return upgradeRefuse, fmt.Sprintf("Latest release %s is not newer than running %s; use --force to install it anyway.\n", latest, current)
	}
}
