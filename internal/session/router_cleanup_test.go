package session

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestShutdown_SetsStoppedBeforeSnapshot is the TOCTOU race pin for #1822
// (Option B). Router.shutdown sets r.stopped under r.mu immediately before
// snapshotting r.sessions; spawnSession checks r.stopped under the same r.mu on
// entry. Therefore the gate and the snapshot are mutually exclusive: a
// concurrent GetOrCreate either ran to completion before the snapshot (and is
// thus captured) or observes stopped=true and is rejected with ErrRouterStopped
// before installing anything.
//
// This test hammers GetOrCreate from many goroutines while Shutdown runs and
// asserts the load-bearing post-condition: after Shutdown returns, r.sessions
// contains no entry that was installed AFTER the snapshot was taken. Because the
// router is wired to a bogus CLI binary, no spawn ever succeeds, so the strongest
// observable is that every late caller either gets ErrRouterStopped or a normal
// spawn failure — never a leaked, installed session. Run under -race to catch any
// regression that reads r.stopped outside r.mu or sets it after the snapshot.
func TestShutdown_SetsStoppedBeforeSnapshot(t *testing.T) {
	t.Parallel()

	r := NewRouter(RouterConfig{
		MaxProcs: 64,
		Wrapper:  cli.NewWrapper("/nonexistent/cli-binary", &cli.ClaudeProtocol{}, "claude"),
	})

	const n = 64
	var wg sync.WaitGroup
	wg.Add(n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			key := "feishu:p2p:u" + string(rune('a'+(i%26)))
			// We don't assert on the error here — the contract is checked on
			// r.sessions below. A late caller may legitimately get
			// ErrRouterStopped (post-gate) or a spawn failure (pre-gate, bogus
			// binary). Neither may leave a leaked session behind.
			_, _, _ = r.GetOrCreate(context.Background(), key, AgentOpts{})
		}(i)
	}

	go func() {
		<-start
		r.Shutdown()
	}()

	close(start)
	wg.Wait()

	// Post-condition: no session may survive in r.sessions. Every spawn either
	// failed (bogus binary) or was gated, and Shutdown detached its snapshot.
	// The leak this fix prevents is a fresh session installed AFTER the snapshot
	// that never gets detached — it would show up here.
	r.mu.RLock()
	leaked := len(r.sessions)
	r.mu.RUnlock()
	if leaked != 0 {
		t.Fatalf("r.sessions has %d entries after Shutdown; a spawn raced past the snapshot and leaked a session (#1822 TOCTOU regression)", leaked)
	}

	// The gate is permanent: any spawn after Shutdown is rejected.
	_, _, err := r.GetOrCreate(context.Background(), "feishu:p2p:late", AgentOpts{})
	if !errors.Is(err, ErrRouterStopped) {
		t.Fatalf("post-Shutdown GetOrCreate err = %v, want ErrRouterStopped", err)
	}
}
