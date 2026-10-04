package shim

// Dead-CLI retire: a shim whose CLI exited keeps its socket bound for the
// post-exit reattach window, so a respawn on the same key would be refused by
// ensureSocketFreeForReuse. The respawn path asks that shim to exit first.

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sync"
	"time"
)

const (
	// retireDialBudget bounds the authenticated probe of a bound socket; a
	// responsive shim answers hello in milliseconds.
	retireDialBudget = 3 * time.Second
	// retireSocketWait is how long a retired shim gets to unlink its socket.
	retireSocketWait = 2 * time.Second
	// retireLockPoll is the retry interval while a Reconnect of the same key
	// holds the per-key mutex.
	retireLockPoll = 10 * time.Millisecond
)

// RetireDeadShim shuts down the shim bound to key's socket when that shim's
// hello reports its CLI dead, and reports whether the socket is gone. It dials
// with the state-file token and never touches m.shims, so a shim whose CLI is
// alive (or does not say) is only probed and left running.
func (m *Manager) RetireDeadShim(ctx context.Context, key string) (bool, error) {
	rmu := m.reconnectKey(key)
	if err := lockWithContext(ctx, rmu); err != nil {
		return false, fmt.Errorf("wait for reconnect of the same key: %w", err)
	}
	defer rmu.Unlock()

	// MaxInt64: the buffer is never replayed, the probe only needs the hello.
	handle, err := m.dialFromStateFile(ctx, key, math.MaxInt64)
	if err != nil {
		return false, err
	}
	if alive := handle.Hello.CLIAlive; alive == nil || *alive {
		handle.Close()
		return false, nil
	}
	slog.Info("retiring dead-CLI shim before respawn",
		"key_hash", KeyHash(key), "shim_pid", handle.Hello.ShimPID)
	handle.Shutdown()
	if !WaitSocketGone(handle.State.Socket, retireSocketWait) {
		return false, fmt.Errorf("socket %s still bound %v after shutdown", handle.State.Socket, retireSocketWait)
	}
	return true, nil
}

// lockWithContext acquires mu or gives up once ctx ends, so a Reconnect of the
// same key (bounded only by its own dial and hello limits) cannot stretch the
// retire past ctx's budget.
func lockWithContext(ctx context.Context, mu *sync.Mutex) error {
	if mu.TryLock() {
		return nil
	}
	t := time.NewTicker(retireLockPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if mu.TryLock() {
				return nil
			}
		}
	}
}

// prepareSocketForSpawn is StartShim's pre-bind step. When the socket file
// exists, the shim behind it is offered a retire before the liveness check:
// the first connection of a dead-CLI shim's reattach window is the one every
// shim build serves, so the authenticated probe must not queue behind a bare
// dial. A shim still listening afterwards keeps ensureSocketFreeForReuse's
// refusal.
func (m *Manager) prepareSocketForSpawn(ctx context.Context, key, socketPath string) error {
	var retireErr error
	if _, err := os.Lstat(socketPath); err == nil {
		rctx, cancel := context.WithTimeout(ctx, retireDialBudget)
		_, retireErr = m.RetireDeadShim(rctx, key)
		cancel()
	}
	err := ensureSocketFreeForReuse(socketPath)
	if err != nil && retireErr != nil {
		slog.Warn("retire dead-CLI shim failed", "key_hash", KeyHash(key), "err", retireErr)
	}
	return err
}
