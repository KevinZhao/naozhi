//go:build unix

package project

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// fakeGitOnPath puts a /bin/sh "git" running body first and alone on PATH,
// and sets GIT_DIR so a GIT_* variable leaking through makes it exit 0.
func fakeGitOnPath(t *testing.T, body string) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\n[ -n \"${GIT_DIR+x}\" ] && exit 0\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("GIT_DIR", "/elsewhere/.git")
}

// gitLsFilesStub reads the answer from what git lists, not from its exit
// code: a git that fails the way the macOS CLT shim does (stderr, exit 1) is
// trackUnknown. GIT_* variables in naozhi's environment do not reach git.
// The deadline is the test's own so a slow first exec cannot flip the answer.
func TestGitTracksStub_FakeGit(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       trackState
	}{
		{"listed", `printf '.naozhi/project.yaml\0'`, trackTracked},
		{"nothing listed", `exit 0`, trackUntracked},
		{"shim exit 1", `echo "xcrun: error: invalid active developer path" >&2; exit 1`, trackUnknown},
		{"not a repo", `echo "fatal: not a git repository" >&2; exit 128`, trackUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeGitOnPath(t, tc.body)
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			if got := gitLsFilesStub(ctx, t.TempDir()); got != tc.want {
				t.Errorf("gitLsFilesStub = %d; want %d", got, tc.want)
			}
		})
	}
}

// A git that has listed the path but never exits is killed when ctx ends and
// reads as trackUnknown, so the sweep keeps the file. ctx is cancelled once
// the script has written its listing, through a FIFO.
func TestGitTracksStub_HungGitIsKilled(t *testing.T) {
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(t.TempDir(), "listed")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	fakeGitOnPath(t, "printf '.naozhi/project.yaml\\0'; echo > '"+fifo+"'; exec "+sleepBin+" 30")
	// O_RDWR opens without waiting for a writer and lets the test itself
	// release the reader when the script exits before writing.
	listed, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer listed.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cancelled := make(chan time.Time, 1)
	go func() {
		_, _ = listed.Read(make([]byte, 1))
		cancel()
		cancelled <- time.Now()
	}()
	got := gitLsFilesStub(ctx, t.TempDir())
	_, _ = listed.Write([]byte("\n"))
	if since := time.Since(<-cancelled); since > 10*time.Second {
		t.Errorf("gitLsFilesStub returned %v after cancel; the hung git was not killed", since)
	}
	if got != trackUnknown {
		t.Errorf("gitLsFilesStub = %d; want trackUnknown (%d)", got, trackUnknown)
	}
}
