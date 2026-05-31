package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/naozhi/naozhi/internal/selfupdate"
)

func runUpgrade(args []string) {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	checkOnly := fs.Bool("check-only", false, "check for a newer version without downloading")
	noRestart := fs.Bool("no-restart", false, "skip service restart after upgrade")
	force := fs.Bool("force", false, "allow upgrading from a dev build to a release")
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

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 1. Resolve latest release tag.
	fmt.Printf("Checking latest release…\n")
	rel, err := selfupdate.LatestRelease(ctx)
	if err != nil {
		fatalf("check latest release: %v\n", err)
	}

	// 2. Compare with running version.
	if rel.Tag == version {
		fmt.Printf("Already at the latest version (%s).\n", version)
		return
	}

	if version == "dev" {
		if !*force {
			fmt.Fprintf(os.Stderr, "Running a dev build. Use --force to replace it with release %s.\n", rel.Tag)
			os.Exit(1)
		}
		fmt.Printf("dev build — upgrading to %s (--force)\n", rel.Tag)
	} else {
		fmt.Printf("New version available: %s → %s\n", version, rel.Tag)
	}

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
	newBin, err := selfupdate.Download(ctx, rel, tmp)
	if err != nil {
		fatalf("download: %v\n", err)
	}
	fmt.Printf("Checksum verified.\n")

	// 5. Replace binary (atomic: stage → rename).
	fmt.Printf("Installing to %s…\n", selfPath)
	backupPath, err := selfupdate.Replace(newBin, selfPath)
	if err != nil {
		fatalf("replace binary: %v\n", err)
	}

	// 6. Restart service (unless skipped or not running).
	//
	// We deliberately do NOT roll back the binary when the restart step
	// returns an error. The binary is already SHA-256 verified and made
	// executable by Replace, so a restart problem is almost always a slow
	// cold start (Type=notify + a loaded host taking longer than
	// TimeoutStartSec to send READY=1) rather than a bad binary. Rolling
	// back a healthy binary on that false signal is exactly what broke the
	// v0.0.27 upgrade. systemd's Restart=always keeps bringing the new
	// binary up; the operator just needs to be told to check on it.
	serviceWasRunning := selfupdate.ServiceRunning()
	restartWarned := false
	if !*noRestart && serviceWasRunning {
		fmt.Printf("Restarting service…\n")
		if err := selfupdate.RestartService(ctx); err != nil {
			restartWarned = true
			fmt.Fprintf(os.Stderr, "\nwarning: service restart could not be confirmed: %v\n", err)
			fmt.Fprintf(os.Stderr, "The new binary IS installed and verified. naozhi may still be starting\n")
			fmt.Fprintf(os.Stderr, "(slow cold start) OR may be failing to start. Check which:\n")
			fmt.Fprintf(os.Stderr, "    systemctl status naozhi\n")
			fmt.Fprintf(os.Stderr, "    journalctl -u naozhi -n 50 --no-pager\n")
			// The backup is a 0600, non-executable copy of the PRIOR binary
			// (copyFileBackup), so a bare `cp` back reproduces the 203/EXEC
			// failure — the restore MUST re-apply the executable bit. There is
			// no `naozhi upgrade --tag` to downgrade with, so spell out the
			// manual path explicitly.
			fmt.Fprintf(os.Stderr, "To roll back to the previous binary:\n")
			fmt.Fprintf(os.Stderr, "    sudo cp %s %s && sudo chmod 0755 %s && sudo systemctl restart naozhi\n",
				backupPath, selfPath, selfPath)
		}
	} else if !serviceWasRunning {
		fmt.Printf("Service not running — skipping restart.\n")
	}

	// 7. Clean up backup on success. Keep it when the restart was not
	// confirmed so the operator has a manual rollback artifact.
	if !restartWarned {
		_ = os.Remove(backupPath)
	}

	fmt.Printf("\n✓ naozhi upgraded to %s\n", rel.Tag)

	if *noRestart || !serviceWasRunning {
		fmt.Printf("  Restart the service manually to apply the new binary.\n")
	}
}
