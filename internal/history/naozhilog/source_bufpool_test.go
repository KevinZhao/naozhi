package naozhilog

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestSource_BufReaderPool_ConcurrentReads pins R20260603-PERF-4: the pooled
// 64 KiB bufio.Reader (shared by decodeFrom / readAllEntries) must Reset(f)
// before use and Reset(nil) on return so concurrent LoadLatest/LoadBefore
// calls never observe a stale buffer or pin a closed fd. Run with -race to
// catch any pooled-reader aliasing. Every concurrent read of the same log
// must return the identical, fully decoded entry set.
func TestSource_BufReaderPool_ConcurrentReads(t *testing.T) {
	p, src, sink, _ := newPersister(t, "pool-key")

	const nEntries = 40
	for i := 0; i < nEntries; i++ {
		persistOne(t, sink, cli.EventEntry{
			UUID:    "uuid" + string(rune('A'+i%26)) + string(rune('0'+i%10)),
			Time:    int64(100 + i),
			Type:    "text",
			Summary: "row",
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Baseline read.
	want, err := src.LoadLatest(context.Background(), 500)
	if err != nil {
		t.Fatalf("baseline LoadLatest: %v", err)
	}
	if len(want) != nEntries {
		t.Fatalf("baseline got %d entries, want %d", len(want), nEntries)
	}

	var wg sync.WaitGroup
	const goroutines = 16
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for r := 0; r < 8; r++ {
				got, err := src.LoadLatest(context.Background(), 500)
				if err != nil {
					t.Errorf("concurrent LoadLatest: %v", err)
					return
				}
				if len(got) != len(want) {
					t.Errorf("concurrent read got %d entries, want %d", len(got), len(want))
					return
				}
				for i := range want {
					if got[i].UUID != want[i].UUID || got[i].Time != want[i].Time {
						t.Errorf("entry[%d]=%+v, want UUID=%q Time=%d",
							i, got[i], want[i].UUID, want[i].Time)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}
