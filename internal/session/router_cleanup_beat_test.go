package session

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/monitor/beat"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// cleanupBeatClock is a never-sleeping beat clock the test advances while the
// loop goroutine beats on it.
type cleanupBeatClock struct {
	mu   sync.Mutex
	mono time.Duration
}

func (c *cleanupBeatClock) Now() time.Time { return time.Unix(0, 0).Add(c.Mono()) }
func (c *cleanupBeatClock) Mono() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mono
}
func (c *cleanupBeatClock) advance(d time.Duration) {
	c.mu.Lock()
	c.mono += d
	c.mu.Unlock()
}

// panicCleanupProc panics in every Cleanup pass, as a bug would.
type panicCleanupProc struct {
	*fakeProcess
	calls atomic.Int32
}

func (p *panicCleanupProc) LastEventAt() time.Time {
	p.calls.Add(1)
	panic("injected cleanup panic")
}

// The cleanup loop beats every iteration; once it gives up after repeated
// panics its heartbeat stops, which /health reports as stale.
func TestCleanupLoop_Heartbeat(t *testing.T) {
	const interval = 10 * time.Millisecond
	past := beat.StartupGrace + time.Hour

	t.Run("beats", func(t *testing.T) {
		c := &cleanupBeatClock{}
		beats := beat.New(c)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		newTestRouter(3).StartCleanupLoop(ctx, interval, beats)
		c.advance(past)
		testhelper.Eventually(t, func() bool { return beats.Stale() == nil },
			2*time.Second, "cleanup loop never beat")
	})

	t.Run("stops beating after giving up", func(t *testing.T) {
		c := &cleanupBeatClock{}
		beats := beat.New(c)
		r := newTestRouter(3)
		proc := &panicCleanupProc{fakeProcess: newIdleProc()}
		injectSession(r, "key1", proc)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// Start on the last allowed attempt so the first panic gives up.
		r.startCleanupLoop(ctx, interval, beats.Register(cleanupLoopBeat, interval), cleanupLoopMaxRestarts-1)
		testhelper.Eventually(t, func() bool { return proc.calls.Load() > 0 },
			2*time.Second, "cleanup never ran")
		c.advance(past)
		time.Sleep(10 * interval)
		if got := beats.Stale(); !slices.Equal(got, []string{cleanupLoopBeat}) {
			t.Errorf("Stale() = %v, want [%s]", got, cleanupLoopBeat)
		}
		if n := proc.calls.Load(); n != 1 {
			t.Errorf("Cleanup ran %d times after giving up, want 1", n)
		}
	})
}
