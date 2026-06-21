package cron

// Tests for R20260616-SEC-8 (#2144): writeSandboxPending must refuse to
// MkdirAll/write through a symlink planted at the sandboxpending directory.

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteSandboxPending_SymlinkDirRefused(t *testing.T) {
	tests := []struct {
		name string
		// setup arranges the on-disk state for the pending-dir path and
		// returns the redirect target dir (where a follow-through write would
		// have landed) when relevant.
		setup func(t *testing.T, storeDir, pendingDir string) (target string)
		// wantRefused: the write must be refused (returns "" path).
		wantRefused bool
	}{
		{
			name: "symlink to sibling dir is refused",
			setup: func(t *testing.T, storeDir, pendingDir string) string {
				target := filepath.Join(storeDir, "evil-target")
				if err := os.MkdirAll(target, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, pendingDir); err != nil {
					t.Fatal(err)
				}
				return target
			},
			wantRefused: true,
		},
		{
			name: "dangling symlink is refused",
			setup: func(t *testing.T, storeDir, pendingDir string) string {
				if err := os.Symlink(filepath.Join(storeDir, "does-not-exist"), pendingDir); err != nil {
					t.Fatal(err)
				}
				return ""
			},
			wantRefused: true,
		},
		{
			name: "plain absent dir is created and written (happy path)",
			setup: func(t *testing.T, storeDir, pendingDir string) string {
				return "" // no symlink: MkdirAll should create it
			},
			wantRefused: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			storeDir := t.TempDir()
			storePath := filepath.Join(storeDir, "cron_jobs.json")
			s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

			pendingDir := pendingDirOf(storePath)
			target := tc.setup(t, storeDir, pendingDir)

			p := sandboxPending{
				JobID:            "0123456789abcdef",
				RunID:            "feedfacefeedface",
				RuntimeSessionID: "run-feedfacefeedface-1234567890123456789",
				StartedAtMS:      1700000000000,
			}
			got := s.writeSandboxPending(p, slog.Default())

			if tc.wantRefused {
				if got != "" {
					t.Fatalf("writeSandboxPending returned %q; want \"\" (refused through symlink)", got)
				}
				// Ensure nothing was written into the redirect target.
				if target != "" {
					if entries, _ := os.ReadDir(target); len(entries) != 0 {
						t.Fatalf("pending write leaked into symlink target %s: %d entries", target, len(entries))
					}
				}
				return
			}

			if got == "" {
				t.Fatal("writeSandboxPending returned \"\" on the happy path; want a real path")
			}
			if _, err := os.Stat(got); err != nil {
				t.Fatalf("pending file not written at %s: %v", got, err)
			}
		})
	}
}
