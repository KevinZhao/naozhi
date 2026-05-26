package shim

import (
	"sync"
	"testing"
)

// TestMaxWriteLineBytes_RaceFree is a regression for R237-CR-4. Before the
// fix, `maxWriteLineBytes` was a plain package-level int that tests overrode
// via direct assignment while handleClient read the same variable from
// another goroutine — `go test -race` would flag the data race. After the
// fix the knob is an atomic.Int64; concurrent Load/Store must remain
// race-free under `-race`.
func TestMaxWriteLineBytes_RaceFree(t *testing.T) {
	orig := maxWriteLineBytes.Load()
	defer maxWriteLineBytes.Store(orig)

	var wg sync.WaitGroup
	const writers = 4
	const readers = 4
	const iters = 1000

	wg.Add(writers + readers)
	for i := 0; i < writers; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				maxWriteLineBytes.Store(int64(1024 + id*8 + j%64))
			}
		}(i)
	}
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			var sink int64
			for j := 0; j < iters; j++ {
				sink += maxWriteLineBytes.Load()
			}
			_ = sink
		}()
	}
	wg.Wait()
}
