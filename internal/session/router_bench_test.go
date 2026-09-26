package session

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
)

// Benchmarks for the Router's session-table paths. They are the gate for
// restructuring the Router's lock: a change to what guards the session table,
// the spawn bookkeeping or the per-key picks is compared against these numbers
// before it lands. Each path is measured quiet and under churn, since the lock
// only costs anything when writers run beside the readers.
//
// The measured calls (SessionFor, GetOrCreate, ListSessionsWithVersion, Stats)
// and the churn writers (GetOrCreate on a fresh key, SetSessionBackend) are
// public Router methods whose signatures survive a restructure; only
// benchInject reaches into the Router's internals, so it is the one place to
// update.
//
//	go test -run '^$' -bench 'BenchmarkRouter' -benchtime 2s ./internal/session/
//
// GetOrCreate on a missing key runs spawnSession to the Spawn call: with no CLI
// wrapper configured it fails there with ErrNoCLIWrapper, after the full
// reserve / unlock / relock / release round trip, without starting a process.

const (
	benchSessions = 200
	benchKey      = "feishu:direct:bench:general"
)

// benchInject installs a live session for key.
func benchInject(r *Router, key string) {
	r.ss.Lock()
	injectSession(r, key, newIdleProc())
	r.ss.Unlock()
}

// benchRouter builds a Router holding benchSessions live sessions, benchKey
// among them.
func benchRouter(b *testing.B) *Router {
	b.Helper()
	// Router teardown logs at INFO; interleaved with the result lines it
	// breaks benchstat parsing.
	prevLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := NewRouter(RouterConfig{MaxProcs: 1_000_000})
	benchInject(r, benchKey)
	for i := 1; i < benchSessions; i++ {
		benchInject(r, fmt.Sprintf("feishu:direct:bench-%d:general", i))
	}
	b.Cleanup(func() {
		r.Shutdown()
		slog.SetDefault(prevLog)
	})
	return r
}

// startRouterChurn runs writers beside the measured reader: one drives
// GetOrCreate on fresh keys (spawnSession's write-lock round trip), one sets
// per-key backend picks.
func startRouterChurn(b *testing.B, r *Router) {
	b.Helper()
	var wg sync.WaitGroup
	quit := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		ctx := context.Background()
		for i := 0; ; i++ {
			select {
			case <-quit:
				return
			default:
			}
			_, _, _ = r.GetOrCreate(ctx, fmt.Sprintf("feishu:direct:churn-%d:general", i%64), AgentOpts{})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-quit:
				return
			default:
			}
			r.SetSessionBackend(fmt.Sprintf("feishu:direct:pick-%d:general", i%64), "claude")
		}
	}()
	b.Cleanup(func() {
		close(quit)
		wg.Wait()
	})
}

func benchRouterModes(b *testing.B, run func(b *testing.B, r *Router)) {
	for _, churn := range []bool{false, true} {
		name := "quiet"
		if churn {
			name = "churn"
		}
		b.Run(name, func(b *testing.B) {
			r := benchRouter(b)
			if churn {
				startRouterChurn(b, r)
			}
			b.ReportAllocs()
			b.ResetTimer()
			run(b, r)
		})
	}
}

func BenchmarkRouterSessionFor(b *testing.B) {
	benchRouterModes(b, func(b *testing.B, r *Router) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_ = r.SessionFor(benchKey)
			}
		})
	})
}

func BenchmarkRouterGetOrCreateHit(b *testing.B) {
	benchRouterModes(b, func(b *testing.B, r *Router) {
		ctx := context.Background()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_, _, _ = r.GetOrCreate(ctx, benchKey, AgentOpts{})
			}
		})
	})
}

func BenchmarkRouterGetOrCreateSpawnFail(b *testing.B) {
	benchRouterModes(b, func(b *testing.B, r *Router) {
		ctx := context.Background()
		for i := 0; i < b.N; i++ {
			_, _, _ = r.GetOrCreate(ctx, "feishu:direct:missing:general", AgentOpts{})
		}
	})
}

func BenchmarkRouterListSessions(b *testing.B) {
	benchRouterModes(b, func(b *testing.B, r *Router) {
		for i := 0; i < b.N; i++ {
			_, _ = r.ListSessionsWithVersion()
		}
	})
}

func BenchmarkRouterStats(b *testing.B) {
	benchRouterModes(b, func(b *testing.B, r *Router) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_, _ = r.Stats()
			}
		})
	})
}
