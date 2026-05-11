//go:build !linux

package shim

import (
	"time"
)

// Config mirrors the Linux-build struct field-for-field so cmd/naozhi/shim.go
// can construct one and hand it to Run unchanged on every GOOS.
type Config struct {
	Key             string
	SocketPath      string
	StateFile       string
	BufferSize      int
	MaxBufBytes     int64
	IdleTimeout     time.Duration
	WatchdogTimeout time.Duration
	CLIPath         string
	Backend         string
	CLIArgs         []string
	CWD             string
}

// Run is the shim subprocess entrypoint; unsupported off Linux.
func Run(_ Config) error { return ErrUnsupportedPlatform }

// CleanStaleSocket is a no-op stub.
func CleanStaleSocket(_ string) error { return ErrUnsupportedPlatform }

// WaitSocketGone returns true immediately so reconnect paths do not block
// on non-Linux — the socket was never created anyway.
func WaitSocketGone(_ string, _ time.Duration) bool { return true }
