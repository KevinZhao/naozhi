//go:build unix

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/shim"
)

// shimCLIStateDir builds a state dir holding every kind of file the service's
// Discover would delete or signal: corrupt, dead PID, a live PID running a
// different binary (a sleep child), and this test process with its socket gone.
func shimCLIStateDir(t *testing.T) (dir string, foreign *exec.Cmd) {
	t.Helper()
	dir = t.TempDir()
	foreign = exec.Command("sleep", "60")
	if err := foreign.Start(); err != nil {
		t.Skipf("cannot start a foreign-binary child: %v", err)
	}
	t.Cleanup(func() {
		_ = foreign.Process.Kill()
		_ = foreign.Wait()
	})
	if err := os.WriteFile(filepath.Join(dir, "corrupt.json"), []byte("bad json {{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, st := range []shim.State{
		{Key: "t:dead", ShimPID: 999999999, Socket: filepath.Join(dir, "dead.sock")},
		{Key: "t:foreign", ShimPID: foreign.Process.Pid, Socket: filepath.Join(dir, "foreign.sock")},
		{Key: "t:nosock", ShimPID: os.Getpid(), Socket: filepath.Join(dir, "gone.sock")},
	} {
		if err := shim.WriteStateFile(filepath.Join(dir, shim.KeyHash(st.Key)+".json"), st); err != nil {
			t.Fatal(err)
		}
	}
	return dir, foreign
}

func shimDirListing(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s %d %s\n", e.Name(), info.Size(), info.ModTime().Format(time.RFC3339Nano))
	}
	return b.String()
}

// TestShimCLI_LeavesStateDirAlone: `shim list` and `shim stop --key` run from
// a binary other than the service's must not delete anyone's state file nor
// signal any PID; stop refuses a target it cannot identify and exits 1.
func TestShimCLI_LeavesStateDirAlone(t *testing.T) {
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGUSR2)
	defer signal.Stop(sigCh)

	dir, foreign := shimCLIStateDir(t)
	mgr, err := shim.NewManager(shim.ManagerConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	before := shimDirListing(t, dir)

	var out, errb strings.Builder
	if code := listShims(mgr, filepath.Join(dir, "no-config.yaml"), &out, &errb); code != 0 {
		t.Fatalf("listShims = %d, stderr %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "foreign-bin") || !strings.Contains(out.String(), "no-socket") {
		t.Errorf("list output lacks the foreign-bin / no-socket rows:\n%s", out.String())
	}

	out.Reset()
	errb.Reset()
	if code := stopShims(mgr, "t:foreign", false, &out, &errb); code != 1 {
		t.Errorf("stopShims on a foreign-binary target = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "skipped t:foreign") {
		t.Errorf("stop stderr lacks the skip reason:\n%s", errb.String())
	}

	if after := shimDirListing(t, dir); after != before {
		t.Errorf("shim CLI changed the state dir:\nbefore:\n%safter:\n%s", before, after)
	}
	if err := foreign.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("foreign-binary PID is gone after shim stop skipped it: %v", err)
	}
	select {
	case sig := <-sigCh:
		t.Fatalf("shim CLI signalled this process: %v", sig)
	case <-time.After(300 * time.Millisecond):
	}
}
