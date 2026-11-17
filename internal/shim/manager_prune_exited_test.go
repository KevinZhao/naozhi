package shim

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A reconnected shim that has since exited has no reaper to drop its entry;
// at the cap, admission must not count it against maxShims, while a live
// shim's entry still counts.
func TestStartShim_AtCapPrunesExitedReconnectedShims(t *testing.T) {
	// Keep SocketPath (and the stale-socket unlink) off ~/.naozhi/run.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	exited := exec.Command("sh", "-c", "exit 0")
	if err := exited.Run(); err != nil {
		t.Fatalf("run sh: %v", err)
	}
	deadPID := exited.Process.Pid
	if pidAlive(deadPID) {
		t.Skipf("pid %d was reused already", deadPID)
	}

	m := &Manager{
		maxShims:  1,
		stateDir:  t.TempDir(),
		naozhiBin: filepath.Join(t.TempDir(), "missing-naozhi"),
		shims: map[string]*ShimHandle{
			"task:gone": {Hello: ServerMsg{ShimPID: deadPID}},
		},
	}
	_, err := m.StartShimWithBackend(context.Background(), "task:new", "/bin/true", "", nil, t.TempDir(), nil, nil)
	if errors.Is(err, ErrMaxShims) {
		t.Fatalf("spawn refused by a dead shim's entry: %v", err)
	}
	m.mu.Lock()
	_, stale := m.shims["task:gone"]
	m.mu.Unlock()
	if stale {
		t.Error("exited shim's entry still in m.shims")
	}

	m.mu.Lock()
	m.shims["task:live"] = &ShimHandle{Hello: ServerMsg{ShimPID: os.Getpid()}}
	m.mu.Unlock()
	if _, err := m.StartShimWithBackend(context.Background(), "task:new", "/bin/true", "", nil, t.TempDir(), nil, nil); !errors.Is(err, ErrMaxShims) {
		t.Fatalf("spawn with a live shim at the cap: err = %v, want ErrMaxShims", err)
	}
}
