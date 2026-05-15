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
		fmt.Printf("Running a dev build; latest release is %s.\n", rel.Tag)
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

	// 5. Replace binary.
	fmt.Printf("Installing to %s…\n", selfPath)
	backupPath, err := selfupdate.Replace(newBin, selfPath)
	if err != nil {
		fatalf("replace binary: %v\n", err)
	}

	// 6. Restart service (unless skipped or not running).
	if !*noRestart {
		if selfupdate.ServiceRunning() {
			fmt.Printf("Restarting service…\n")
			if err := selfupdate.RestartService(); err != nil {
				// Upgrade succeeded but restart failed — roll back and report.
				fmt.Fprintf(os.Stderr, "error: service restart failed: %v\n", err)
				fmt.Fprintf(os.Stderr, "Rolling back binary…\n")
				if rbErr := selfupdate.Rollback(selfPath, backupPath); rbErr != nil {
					fatalf("rollback failed: %v (original binary backed up at %s)\n", rbErr, backupPath)
				}
				fatalf("upgrade rolled back; fix the service issue and retry\n")
			}
		} else {
			fmt.Printf("Service not running — skipping restart.\n")
		}
	}

	// 7. Clean up backup on success.
	_ = os.Remove(backupPath)

	fmt.Printf("\n✓ naozhi upgraded to %s\n", rel.Tag)

	if *noRestart || !selfupdate.ServiceRunning() {
		fmt.Printf("  Restart the service manually to apply the new binary.\n")
	}
}
