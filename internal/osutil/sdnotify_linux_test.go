//go:build linux

package osutil

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSdNotify_NoSocket verifies that SdNotify returns nil when NOTIFY_SOCKET is unset.
func TestSdNotify_NoSocket(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := SdNotify("READY=1"); err != nil {
		t.Errorf("SdNotify with no socket = %v, want nil", err)
	}
}

// TestSdNotify_FakeSocket starts a real unixgram listener and checks that the
// message arrives. This exercises the full dial+write path on Linux.
func TestSdNotify_FakeSocket(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "notify.sock")

	// Start a unixgram listener before setting NOTIFY_SOCKET so SdNotify can dial.
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sockPath, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen unixgram: %v", err)
	}
	defer conn.Close()

	t.Setenv("NOTIFY_SOCKET", sockPath)

	if err := SdNotify("READY=1"); err != nil {
		t.Fatalf("SdNotify = %v, want nil", err)
	}

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read from notify socket: %v", err)
	}
	got := string(buf[:n])
	if got != "READY=1" {
		t.Errorf("received %q, want \"READY=1\"", got)
	}
}

// TestSdNotify_WatchdogMessage tests sending a WATCHDOG=1 message.
func TestSdNotify_WatchdogMessage(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "watchdog.sock")

	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sockPath, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen unixgram: %v", err)
	}
	defer conn.Close()

	t.Setenv("NOTIFY_SOCKET", sockPath)

	if err := SdNotify("WATCHDOG=1"); err != nil {
		t.Fatalf("SdNotify WATCHDOG=1 = %v, want nil", err)
	}

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "WATCHDOG=1" {
		t.Errorf("received %q, want \"WATCHDOG=1\"", got)
	}
}

// TestSdNotify_BadSocketPath verifies that an invalid socket path returns an error.
func TestSdNotify_BadSocketPath(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "/nonexistent/path/notify.sock")
	err := SdNotify("READY=1")
	if err == nil {
		t.Error("SdNotify with bad path should return error, got nil")
	}
}

// TestSdNotify_EnvVarIsolation ensures each sub-test restores NOTIFY_SOCKET.
func TestSdNotify_EnvVarIsolation(t *testing.T) {
	orig := os.Getenv("NOTIFY_SOCKET")
	defer os.Setenv("NOTIFY_SOCKET", orig)

	os.Setenv("NOTIFY_SOCKET", "")
	if err := SdNotify("STOPPING=1"); err != nil {
		t.Errorf("SdNotify with empty NOTIFY_SOCKET = %v, want nil", err)
	}
}
