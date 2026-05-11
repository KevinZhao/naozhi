//go:build !linux

package shim

import (
	"bufio"
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

// ErrUnsupportedPlatform is returned by every shim/discovery entrypoint on
// non-Linux builds. The shim subsystem relies on /proc, SO_PEERCRED,
// syscall.Umask, SIGUSR2, and busctl — all Linux-only — so macOS and
// Windows builds compile but fail fast at runtime if any code path
// actually exercises them. Production naozhi only ships on Linux; these
// stubs exist so `GOOS=darwin go build ./...` and `GOOS=windows go build
// ./...` succeed in CI.
var ErrUnsupportedPlatform = errors.New("shim/discovery not supported on this platform")

// ErrMaxShims mirrors the Linux build's package-level variable so callers
// that branch on errors.Is(err, ErrMaxShims) compile on every GOOS.
var ErrMaxShims = errors.New("max shims reached")

// Manager stub: zero-value only. All methods return ErrUnsupportedPlatform.
type Manager struct{}

// ShimHandle stub retains the field set referenced by cmd/ and internal/
// callers so struct-literal and field-access sites type-check.
type ShimHandle struct {
	Conn       net.Conn
	Reader     *bufio.Reader
	Writer     *bufio.Writer
	WriteMu    sync.Mutex
	Token      []byte
	State      State
	Hello      ServerMsg
	ClientDone chan struct{}
}

// ManagerConfig mirrors the Linux-build struct field-for-field so call
// sites constructing the config compile unchanged.
type ManagerConfig struct {
	StateDir        string
	CLIPath         string
	IdleTimeout     time.Duration
	WatchdogTimeout time.Duration
	BufferSize      int
	MaxBufBytes     int64
	MaxShims        int
}

// NewManager always errors on non-Linux builds.
func NewManager(_ ManagerConfig) (*Manager, error) {
	return nil, ErrUnsupportedPlatform
}

func (m *Manager) StartShim(_ context.Context, _ string, _ []string, _ string) (*ShimHandle, error) {
	return nil, ErrUnsupportedPlatform
}

func (m *Manager) StartShimWithBackend(_ context.Context, _, _, _ string, _ []string, _ string) (*ShimHandle, error) {
	return nil, ErrUnsupportedPlatform
}

func (m *Manager) Reconnect(_ context.Context, _ string, _ int64) (*ShimHandle, error) {
	return nil, ErrUnsupportedPlatform
}

func (m *Manager) ForceCleanupZombie(_ State) {}

func (m *Manager) Discover() ([]State, error) { return nil, ErrUnsupportedPlatform }

func (m *Manager) StopAll(_ context.Context) {}

func (m *Manager) DetachAll() {}

func (m *Manager) Remove(_ string) {}

func (m *Manager) CLIPath() string { return "" }

func (h *ShimHandle) SendMsg(_ ClientMsg) error         { return ErrUnsupportedPlatform }
func (h *ShimHandle) ReadMsg() (ServerMsg, error)       { return ServerMsg{}, ErrUnsupportedPlatform }
func (h *ShimHandle) DrainReplay() ([]ServerMsg, error) { return nil, ErrUnsupportedPlatform }
func (h *ShimHandle) Close()                            {}
func (h *ShimHandle) Detach()                           {}
func (h *ShimHandle) Shutdown()                         {}
