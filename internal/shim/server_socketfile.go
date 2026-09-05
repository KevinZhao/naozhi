package shim

// Unix-socket file lifecycle: stale-socket cleanup, reuse guards and the
// external-removal watch.

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"
)

// watchSocketFile polls the socket path and initiates shutdown if the file
// disappears (listener fd alive but path gone = unreachable zombie shim).
// interval is parameterised for tests.
func (s *shimServer) watchSocketFile(socketPath string, interval time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("shim watchSocketFile panic recovered",
				"panic", r, "stack", string(debug.Stack()))
		}
	}()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			// Lstat (not Stat): a symlink swap pointing at a still-existing
			// path must not keep this watcher alive past the real socket's removal.
			if _, err := os.Lstat(socketPath); err != nil {
				// Only self-terminate on ENOENT — transient EACCES/ESTALE/EINTR
				// must not take down a healthy shim.
				if !errors.Is(err, os.ErrNotExist) {
					slog.Warn("shim socket stat transient error, staying up",
						"socket", socketPath, "err", err)
					continue
				}
				// Do not recreate the socket here: that would race StartShim's
				// dial-first guard. Exit so naozhi spawns a fresh shim.
				slog.Warn("shim socket file disappeared, shutting down",
					"socket", socketPath, "err", err)
				s.initiateShutdown()
				return
			}
		}
	}
}

func socketDir(socketPath string) string {
	dir := filepath.Dir(socketPath)
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

// --- CLI process management ---

// CleanStaleSocket removes a socket file if no shim is listening on it.
func CleanStaleSocket(path string) error {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		conn.Close()
		return fmt.Errorf("socket %s is alive, not removing", path)
	}
	return os.Remove(path)
}

// ensureSocketFreeForReuse is the StartShim-side pre-bind check: refuse to
// clobber a live listener, since removing its filesystem entry turns the peer
// into an unreachable zombie (fd still held by the kernel). 500ms is generous
// for a unix connect; slower is already diagnostic. Separate from
// CleanStaleSocket, whose shim-side bind path expects a different error surface.

// ensureSocketFreeForReuse is the StartShim-side pre-bind check: refuse to
// clobber a live listener, since removing its filesystem entry turns the peer
// into an unreachable zombie (fd still held by the kernel). 500ms is generous
// for a unix connect; slower is already diagnostic. Separate from
// CleanStaleSocket, whose shim-side bind path expects a different error surface.
func ensureSocketFreeForReuse(socketPath string) error {
	if conn, err := net.DialTimeout("unix", socketPath, 500*time.Millisecond); err == nil {
		_ = conn.Close()
		return fmt.Errorf("shim already listening on %s: refusing to clobber", socketPath)
	}
	_ = os.Remove(socketPath)
	return nil
}

// WaitSocketGone polls the socket path until it disappears or maxWait
// elapses. Returns true when the socket is gone; false on timeout.
//
// Used by callers that just asked a shim to shut down and will spawn a fresh
// one on the same key: observing the unlink avoids the dial-first "refusing
// to clobber" guard. Polls by stat (not dial) so no connection state is
// re-established with a lingering accept goroutine.

// WaitSocketGone polls the socket path until it disappears or maxWait
// elapses. Returns true when the socket is gone; false on timeout.
//
// Used by callers that just asked a shim to shut down and will spawn a fresh
// one on the same key: observing the unlink avoids the dial-first "refusing
// to clobber" guard. Polls by stat (not dial) so no connection state is
// re-established with a lingering accept goroutine.
func WaitSocketGone(socketPath string, maxWait time.Duration) bool {
	if socketPath == "" {
		return true
	}
	deadline := time.Now().Add(maxWait)
	// Fast path: already gone.
	if _, err := os.Stat(socketPath); errors.Is(err, fs.ErrNotExist) {
		return true
	}
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		<-t.C
		if _, err := os.Stat(socketPath); errors.Is(err, fs.ErrNotExist) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}
