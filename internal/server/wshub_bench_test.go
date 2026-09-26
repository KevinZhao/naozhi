package server

import (
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"unsafe"

	"github.com/naozhi/naozhi/internal/node"
)

// Benchmarks for the Hub's subscriber hot paths. They exist as the gate for
// restructuring the Hub's locks: a change to how clients, the authenticated
// set or the per-key counts are guarded is compared against these numbers
// before it lands. Each path is measured quiet and under churn, since the
// locks only cost anything when writers (subscribe, unsubscribe, connect,
// disconnect) run beside the readers.
//
// The measured calls (fanOutToSubscribers, snapshotAuthenticated,
// singleSubscriber, BroadcastSessionsUpdate) keep their signatures across a
// restructure; only the bench* setup helpers below reach into the Hub's
// internals, so they are the one place to update.
//
//	go test -run '^$' -bench 'BenchmarkHub' -benchtime 2s ./internal/server/

const (
	benchClients = 200
	benchKey     = "dashboard:direct:bench:general"
)

// benchAddClient registers an authenticated client. Its send channel is
// unbuffered and its done channel already closed, so every SendRaw takes the
// same constant-cost done branch: no drain goroutines whose wake-ups would
// swamp the lock costs being measured.
func benchAddClient(h *Hub) *wsClient {
	c := &wsClient{
		hub:  h,
		send: make(chan []byte),
		done: make(chan struct{}),
	}
	close(c.done)
	c.authenticated.Store(true)
	h.register(c)
	return c
}

// benchSubscribe holds a subscription slot for key, as a subscribe in flight
// does; fan-out delivers to it like to an installed one.
func benchSubscribe(h *Hub, c *wsClient, key string) {
	h.subs.reserve(c, key)
}

func benchUnsubscribe(h *Hub, c *wsClient, key string) {
	h.subs.release(c, key)
}

// benchHub builds a Hub with benchClients authenticated clients, the first
// subs of them subscribed to benchKey.
func benchHub(b *testing.B, subs int) *Hub {
	b.Helper()
	// Router and Hub teardown log at INFO; interleaved with the result lines
	// they break benchstat parsing.
	prevLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	h, _ := newTestHub("")
	for i := 0; i < benchClients; i++ {
		c := benchAddClient(h)
		if i < subs {
			benchSubscribe(h, c, benchKey)
		}
	}
	b.Cleanup(func() {
		h.Shutdown()
		slog.SetDefault(prevLog)
	})
	return h
}

// startChurn runs writers beside the measured reader: one toggles
// subscriptions on other keys, one connects and disconnects clients. Both
// take the Hub's write locks, which is what a lock restructure changes.
func startChurn(b *testing.B, h *Hub) {
	b.Helper()
	var wg sync.WaitGroup
	quit := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		c := benchAddClient(h)
		for i := 0; ; i++ {
			select {
			case <-quit:
				return
			default:
			}
			key := fmt.Sprintf("churn-%d", i%16)
			benchSubscribe(h, c, key)
			benchUnsubscribe(h, c, key)
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-quit:
				return
			default:
			}
			c := benchAddClient(h)
			h.unregister(c)
		}
	}()
	b.Cleanup(func() {
		close(quit)
		wg.Wait()
	})
}

func benchModes(b *testing.B, run func(b *testing.B, churn bool)) {
	for _, churn := range []bool{false, true} {
		name := "quiet"
		if churn {
			name = "churn"
		}
		b.Run(name, func(b *testing.B) { run(b, churn) })
	}
}

func BenchmarkHubFanOutToSubscribers(b *testing.B) {
	frame := func() any { return map[string]string{"type": "session_system_event", "key": benchKey} }
	for _, subs := range []int{1, 50} {
		b.Run(fmt.Sprintf("subs=%d", subs), func(b *testing.B) {
			benchModes(b, func(b *testing.B, churn bool) {
				h := benchHub(b, subs)
				if churn {
					startChurn(b, h)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					h.fanOutToSubscribers(benchKey, frame)
				}
			})
		})
	}
}

func BenchmarkHubSnapshotAuthenticated(b *testing.B) {
	benchModes(b, func(b *testing.B, churn bool) {
		h := benchHub(b, 0)
		if churn {
			startChurn(b, h)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ptr, snap := h.snapshotAuthenticated()
			releaseBroadcastSnap(ptr, snap)
		}
	})
}

func BenchmarkHubSingleSubscriber(b *testing.B) {
	benchModes(b, func(b *testing.B, churn bool) {
		h := benchHub(b, 1)
		if churn {
			startChurn(b, h)
		}
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_ = h.singleSubscriber(benchKey)
			}
		})
	})
}

func BenchmarkHubBroadcastSessionsUpdate(b *testing.B) {
	benchModes(b, func(b *testing.B, churn bool) {
		h := benchHub(b, 0)
		if churn {
			startChurn(b, h)
		}
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				h.BroadcastSessionsUpdate()
			}
		})
	})
}

// BenchmarkHubSubscribeCycle is a subscribe that finds no session: the slot is
// reserved against both caps and released. Its cost must not grow with the
// number of connected clients (benchClients here); a scan of every client
// under the lock would show up as a multiple of the quiet figure.
func BenchmarkHubSubscribeCycle(b *testing.B) {
	benchModes(b, func(b *testing.B, churn bool) {
		h := benchHub(b, 0)
		if churn {
			startChurn(b, h)
		}
		c := benchAddClient(h)
		msg := node.ClientMsg{Type: "subscribe", Key: "test:d:u:missing"}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			h.handleSubscribe(c, msg)
		}
	})
}

// TestHub_AuthMuOffTheMuCacheLine keeps the padding between the registry's
// mu and authMu from being lost to a field reshuffle: the benchmark only catches
// it when someone runs it.
func TestHub_AuthMuOffTheMuCacheLine(t *testing.T) {
	var r subscriberRegistry
	if d := unsafe.Offsetof(r.authMu) - unsafe.Offsetof(r.mu); d < 128 {
		t.Errorf("authMu is %d bytes after mu, want >= 128 so the two locks never share a cache line", d)
	}
}
