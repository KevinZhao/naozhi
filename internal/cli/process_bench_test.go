package cli

import (
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// Process benchmarks: the read loop's per-frame ingest and the dashboard's
// snapshot reads, each quiet and with the other side running. They guard the
// split of Process into its owners: a restructure that adds a lock round
// trip or an allocation per frame shows up here before it lands.
//
//	go test -run '^$' -bench 'BenchmarkProcess' -benchmem ./internal/cli/
//
// Frames go through handleShimStdout (protocol parse → HandleEvent →
// dispatchProtocolEvent) the way readLoop hands them over, without the socket;
// kiro-style metadata frames go straight to dispatchProtocolEvent, since the
// claude protocol never produces them.

const benchAssistantFrame = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"streaming a chunk of the answer"}]}}`

var benchMetadata = clievent.Event{
	Type: "metadata",
	Metadata: &clievent.EventMetadata{
		ContextUsagePercent: 42,
		TurnDurationMs:      1200,
		MeteringUsage:       []clievent.MeteringEntry{{Value: 0.5, Unit: "credit", UnitPlural: "credits"}},
		Effort:              "high",
	},
}

// benchProcess is a Process mid-turn, with a consumer draining the event
// channel the way an active Send does.
func benchProcess(b *testing.B) (*Process, *slog.Logger) {
	b.Helper()
	p, srv := shimTestPair(&ClaudeProtocol{})
	p.turn.mu.Lock()
	p.turn.state = StateRunning
	p.turn.mu.Unlock()
	quit := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-p.eventCh:
			case <-quit:
				return
			}
		}
	}()
	b.Cleanup(func() {
		close(quit)
		wg.Wait()
		srv.Close()
	})
	return p, slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ingestOne feeds frame i of a turn-shaped stream: mostly assistant chunks,
// with a result (cost) and a metadata frame (metering) every 32 frames.
func ingestOne(p *Process, log *slog.Logger, i int) {
	switch i % 32 {
	case 30:
		p.handleShimStdout(shimMsg{Type: "stdout", Seq: int64(i), Line: resultFrameWithModelUsage}, log)
		p.turn.mu.Lock()
		p.turn.state = StateRunning // the next turn
		p.turn.mu.Unlock()
	case 31:
		p.dispatchProtocolEvent(benchMetadata, log)
	default:
		p.handleShimStdout(shimMsg{Type: "stdout", Seq: int64(i), Line: benchAssistantFrame}, log)
	}
}

// readSnapshot is what a dashboard snapshot reads from a live process.
func readSnapshot(p *Process) {
	_ = p.State()
	_ = p.SessionID()
	_ = p.TotalCost()
	_ = p.MeteringUsage()
	_ = p.ContextUsagePercent()
	_ = p.Model()
	_ = p.Effort()
}

func benchModes(b *testing.B, run func(b *testing.B, p *Process, log *slog.Logger), background func(p *Process, log *slog.Logger, i int)) {
	for _, busy := range []bool{false, true} {
		name := "quiet"
		if busy {
			name = "busy"
		}
		b.Run(name, func(b *testing.B) {
			p, log := benchProcess(b)
			if busy {
				quit := make(chan struct{})
				var wg sync.WaitGroup
				for w := 0; w < 2; w++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for i := 0; ; i++ {
							select {
							case <-quit:
								return
							default:
							}
							background(p, log, i)
						}
					}()
				}
				b.Cleanup(func() {
					close(quit)
					wg.Wait()
				})
			}
			b.ReportAllocs()
			b.ResetTimer()
			run(b, p, log)
		})
	}
}

// BenchmarkProcessIngest: per-frame cost of the read loop; busy adds
// dashboard readers.
func BenchmarkProcessIngest(b *testing.B) {
	benchModes(b, func(b *testing.B, p *Process, log *slog.Logger) {
		for i := 0; i < b.N; i++ {
			ingestOne(p, log, i)
		}
	}, func(p *Process, _ *slog.Logger, _ int) { readSnapshot(p) })
}

// BenchmarkProcessSnapshotRead: a dashboard snapshot's reads; busy adds a
// read loop ingesting frames. The background ingest is a single writer, as
// readLoop is.
func BenchmarkProcessSnapshotRead(b *testing.B) {
	var writer sync.Mutex
	benchModes(b, func(b *testing.B, p *Process, _ *slog.Logger) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				readSnapshot(p)
			}
		})
	}, func(p *Process, log *slog.Logger, i int) {
		// Two background goroutines, one writer: readLoop never runs twice.
		if !writer.TryLock() {
			return
		}
		defer writer.Unlock()
		ingestOne(p, log, i)
	})
}
