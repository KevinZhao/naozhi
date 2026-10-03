//go:build linux || darwin

package shim

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestReconnect_BinaryMismatchDoesNotSignal pins that Reconnect drops the
// state file of a PID whose binary does not match but never signals it: the
// PID may belong to an unrelated process after PID reuse.
func TestReconnect_BinaryMismatchDoesNotSignal(t *testing.T) {
	dir := t.TempDir()
	m := mustNewManager(t, ManagerConfig{StateDir: dir})
	m.naozhiBin = "/nonexistent/not-naozhi-binary"

	key := "test:reconnect-mismatch"
	keyHash := KeyHash(key)
	statePath := StateFilePath(dir, keyHash)
	if err := WriteStateFile(statePath, State{
		ShimPID:   os.Getpid(), // live PID, but "wrong binary"
		Socket:    SocketPath(keyHash),
		AuthToken: "dA==",
		Key:       key,
	}); err != nil {
		t.Fatal(err)
	}

	// We are the target PID: without a handler SIGUSR2 would kill the test
	// binary instead of failing an assertion.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR2)
	defer signal.Stop(sigCh)

	_, err := m.Reconnect(context.Background(), key, 0)
	if err == nil || !strings.Contains(err.Error(), "binary mismatch") {
		t.Fatalf("Reconnect err = %v, want the binary-mismatch rejection", err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state file should be removed on mismatch, stat err = %v", err)
	}
	select {
	case <-sigCh:
		t.Error("Reconnect sent SIGUSR2 to a PID whose binary did not match")
	case <-time.After(500 * time.Millisecond):
	}
}
