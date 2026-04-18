//go:build linux

package osutil

import (
	"net"
	"os"
	"time"
)

// SdNotify sends a notification to systemd via the NOTIFY_SOCKET.
// Common states: "READY=1", "WATCHDOG=1", "STOPPING=1".
// Returns nil silently when not running under systemd (no NOTIFY_SOCKET).
func SdNotify(state string) error {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return nil
	}
	conn, err := net.DialTimeout("unixgram", sock, time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, err = conn.Write([]byte(state))
	return err
}
