package persist

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// loop.go — the run goroutine: the select loop, the op protocol it serves, and
// shutdown. Extracted from persister.go (J9 of #2548). Everything here executes
// on the single run goroutine, which is what lets the scratch buffers in
// flushScratch be reused without synchronisation.

var (
	ErrPersisterClosed = errors.New("persist: persister closed")
)

// ----- internal ops channel -------------------------------------

type opKind int

const (
	opDrop opKind = iota
	opFlushAll
	// opDropDone is posted by the async removeKeyFiles goroutine once the
	// unlink completes; it carries the dropState channel so the run
	// goroutine only retires the entry that still matches (#1774).
	opDropDone
)

type op struct {
	kind opKind
	key  string
	stem string
	done chan error
	// ch is the per-stem completion channel; set only on opDropDone.
	ch chan struct{}
}

// run is the single writer goroutine's main loop: batch writes from `in`,
// control ops from `opCh`, the debounce ticker and the idle sweeper. One
// goroutine owns p.writers and every perKeyWriter, so no locks are needed.
func (p *Persister) run() {
	defer p.wg.Done()

	// Debounce tick at FlushInterval/2: worst-case wait ~1.5× FlushInterval.
	flushTick := p.opts.FlushInterval / 2
	if flushTick < 10*time.Millisecond {
		flushTick = 10 * time.Millisecond
	}
	flushT := time.NewTicker(flushTick)
	defer flushT.Stop()

	// Idle sweeper closes fds not written to recently.
	idleTick := p.opts.IdleCloseAfter / 4
	if idleTick < 30*time.Second {
		idleTick = 30 * time.Second
	}
	idleT := time.NewTicker(idleTick)
	defer idleT.Stop()

	for {
		select {
		case job := <-p.in:
			// One Clock() per batch shared by handleBatch and lastDrainNS.
			now := p.opts.Clock()
			p.handleBatch(job, now)
			p.lastDrainNS.Store(now.UnixNano())

		case o := <-p.opCh:
			p.handleOp(o)

		case <-flushT.C:
			p.tickFlush()

		case <-idleT.C:
			p.tickIdleClose()

		case <-p.closeCh:
			// Drain remaining in-flight batches.
			for {
				select {
				case job := <-p.in:
					p.handleBatch(job, p.opts.Clock())
				default:
					goto drainOps
				}
			}
		drainOps:
			// Fail pending DropKey/Flush ops with ErrPersisterClosed so a caller
			// that raced past the closed guard into opCh returns promptly rather
			// than waiting out its ctx. opCh is buffered, so a bounded loop suffices.
			for {
				select {
				case o := <-p.opCh:
					if o.kind == opDropDone {
						// Unlink finished during shutdown: replay its deferred batches
						// so a clean Stop does not lose them. Match-and-delete as live.
						if cur, ok := p.dropping[o.stem]; ok && cur.done == o.ch {
							pending := cur.pending
							delete(p.dropping, o.stem)
							for _, job := range pending {
								p.handleBatch(job, p.opts.Clock())
							}
						}
						continue
					}
					if o.done != nil {
						// Buffered (cap=1) so this never blocks.
						o.done <- ErrPersisterClosed
					}
				default:
					// Replay batches still deferred behind an unlink whose opDropDone
					// never landed (its goroutine took the closeCh branch); recreating
					// the file matches the live drop-then-recreate path and frees arenas.
					p.replayDroppingPending()
					p.shutdownAll()
					return
				}
			}
		}
	}
}

// replayDroppingPending drains every dropState.pending FIFO into
// handleBatch during Stop's shutdown drain so deferred batches are
// persisted (and their arenas returned). Run-goroutine only (#1848).
func (p *Persister) replayDroppingPending() {
	if len(p.dropping) == 0 {
		return
	}
	// Snapshot, then delete every dropping entry BEFORE replaying: handleBatch
	// gates on p.dropping[stem] and would otherwise re-defer into the same
	// dropState. A late opDropDone then finds no entry and is a no-op.
	type stemPending struct {
		stem    string
		pending []batchJob
	}
	snapshot := make([]stemPending, 0, len(p.dropping))
	for stem, ds := range p.dropping {
		snapshot = append(snapshot, stemPending{stem: stem, pending: ds.pending})
	}
	for _, sp := range snapshot {
		delete(p.dropping, sp.stem)
	}
	for _, sp := range snapshot {
		for _, job := range sp.pending {
			p.handleBatch(job, p.opts.Clock())
		}
	}
}

// shutdownAll flushes then closes every writer so a clean Stop loses no
// debounce window. Fan-out is parallel (#1408): each perKeyWriter is
// independent and the only shared state flush()/close() touch is the
// fsyncCnt atomic and the thread-safe Observer. The map is copied to
// slices first so workers never touch p.writers concurrently.
func (p *Persister) shutdownAll() {
	if len(p.writers) == 0 {
		return
	}
	keys := make([]string, 0, len(p.writers))
	ws := make([]*perKeyWriter, 0, len(p.writers))
	for k, w := range p.writers {
		keys = append(keys, k)
		ws = append(ws, w)
	}
	p.parallelFsync(keys, ws, func(k string, w *perKeyWriter) {
		if err := w.flush(p); err != nil {
			slog.Warn("event log persist: flush on shutdown failed",
				"key", k, "err", err)
		}
		if err := w.close(); err != nil {
			slog.Warn("event log persist: close on shutdown failed",
				"key", k, "err", err)
		}
	})
	for _, k := range keys {
		delete(p.writers, k)
	}
}

// parallelFsyncMaxWorkers caps the fsync worker pool used by shutdownAll /
// flushAllLocked / tickFlush; workers block in fsync for tens of ms on
// slow disks, so a small bounded pool is intentional.
const parallelFsyncMaxWorkers = 8

// parallelFsyncWorkers is a test hook pinning the worker count (1 for
// deterministic ordering); 0 = auto-size up to parallelFsyncMaxWorkers.
var parallelFsyncWorkers = 0

// parallelFsync fans fn over (keys[i], ws[i]) with a bounded worker pool;
// 1-2 writers run serially. fn MUST NOT mutate state shared across writer
// indices (persister-global state must be atomic / mutex-guarded). Joins
// all workers before returning so callers may mutate p.writers afterward.
func (p *Persister) parallelFsync(keys []string, ws []*perKeyWriter, fn func(string, *perKeyWriter)) {
	n := len(ws)
	if n == 0 {
		return
	}
	if n <= 2 {
		// Serial: goroutine + WaitGroup setup costs more than two fsyncs.
		for i := range ws {
			fn(keys[i], ws[i])
		}
		return
	}
	workers := parallelFsyncWorkers
	if workers <= 0 {
		workers = parallelFsyncMaxWorkers
	}
	if workers > n {
		workers = n
	}
	if workers == 1 {
		for i := range ws {
			fn(keys[i], ws[i])
		}
		return
	}
	var idx atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("persist: parallelFsync worker panic", "panic", r)
				}
			}()
			for {
				i := idx.Add(1) - 1
				if i >= int64(n) {
					return
				}
				fn(keys[i], ws[i])
			}
		}()
	}
	wg.Wait()
}

func (p *Persister) handleOp(o op) {
	switch o.kind {
	case opDrop:
		// Drain p.in first so the drop observes every prior write for this
		// key; otherwise an in-flight batch could land AFTER the remove and
		// recreate the files.
		p.drainInChannel()
		// Synchronous in-memory phase (close fd + delete map entry), then an
		// async unlink so a slow FUSE/NFS os.Remove never stalls the writer
		// goroutine (#1284).
		p.dropInMemoryLocked(o.key)
		// Publish the dropState BEFORE spawning the unlink: handleBatch defers
		// batches for this stem into ds.pending and opDropDone replays them
		// after the unlink, so no O_CREATE for the stem precedes the remove
		// (#1774, #1848).
		ds := &dropState{done: make(chan struct{})}
		p.dropping[o.stem] = ds
		go func(stem string, done chan error, ch chan struct{}) {
			err := p.removeKeyFiles(stem)
			// Close first, then post opDropDone carrying ch so the replay only
			// fires if this exact dropState is still installed.
			close(ch)
			select {
			case p.opCh <- op{kind: opDropDone, stem: stem, ch: ch}:
			case <-p.closeCh:
				// Shutting down: Stop's drain replays or releases deferred batches.
			}
			if done != nil {
				done <- err
			}
		}(o.stem, o.done, ds.done)
		return
	case opDropDone:
		// Retire only if the entry still matches our channel; a newer opDrop
		// may have installed a fresh dropState in between.
		cur, ok := p.dropping[o.stem]
		if !ok || cur.done != o.ch {
			return
		}
		// Delete BEFORE replaying so the replayed writerFor opens the
		// recreated file instead of re-deferring into the retired dropState.
		pending := cur.pending
		delete(p.dropping, o.stem)
		for _, job := range pending {
			p.handleBatch(job, p.opts.Clock())
		}
		if len(pending) > 0 {
			p.lastDrainNS.Store(p.opts.Clock().UnixNano())
		}
		return
	case opFlushAll:
		// Flush must observe every queued batchJob before fsyncing.
		p.drainInChannel()
		err := p.flushAllLocked()
		if o.done != nil {
			o.done <- err
		}
	}
}

// drainInChannel writes every queued batchJob until p.in is empty.
// Run-goroutine only (via handleOp).
func (p *Persister) drainInChannel() {
	// Refresh `now` every drainClockRefreshEvery batches: one Clock() per
	// batch is wasteful, but one per drain would stamp late writers'
	// lastActivity with the pre-drain instant and trip tickIdleClose (#1525).
	var now time.Time
	drained := false
	sinceRefresh := 0
	for {
		select {
		case job := <-p.in:
			if !drained || sinceRefresh >= drainClockRefreshEvery {
				now = p.opts.Clock()
				sinceRefresh = 0
			}
			p.handleBatch(job, now)
			drained = true
			sinceRefresh++
		default:
			if drained {
				p.lastDrainNS.Store(now.UnixNano())
			}
			return
		}
	}
}

// drainClockRefreshEvery bounds how stale drainInChannel's `now` may get
// so a long burst cannot make tickIdleClose misjudge a late writer (#1525).
const drainClockRefreshEvery = 16

// dropInMemoryLocked closes the per-key writer and removes its map entry.
// Must NOT touch the filesystem beyond the fd close so the op stays fast
// on slow filesystems (#1284).
