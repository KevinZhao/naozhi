package persist

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// recordBufPool reuses the bytes.Buffer schema.MarshalRecordInto writes
// into so handleBatch avoids json's per-call encodeState alloc. Reset
// before Put; capped by recordBufMaxCap on return.
type Observer interface {
	// OnWrite is called once per EventEntry that reaches disk.
	OnWrite(n int)
	// OnDrop is called once per EventEntry dropped (channel full etc).
	OnDrop(n int)
	// OnFsync is called each time the persister fsyncs log or idx.
	OnFsync()
	// OnMalformed is called when schema.MarshalRecord rejects an entry.
	OnMalformed()
	// OnReplayLeak is called with the batch size when a replayPhase=true
	// batch reaches the sink (SetPersistSink-after-InjectHistory violated).
	OnReplayLeak(n int)
}

// noopObserver discards every counter tick; used when Options.Observer is nil.
type noopObserver struct{}

func (noopObserver) OnWrite(int)      {}
func (noopObserver) OnDrop(int)       {}
func (noopObserver) OnFsync()         {}
func (noopObserver) OnMalformed()     {}
func (noopObserver) OnReplayLeak(int) {}

// Options configures a Persister. Defaults apply for zero-valued
// fields so callers only have to set what they want to override.
type Options struct {
	// Dir holds the <keyhash>.log / <keyhash>.idx files. Required.
	Dir string

	// MaxFileBytes triggers rotate when a log grows past it. 0 → DefaultMaxFileBytes.
	MaxFileBytes int64

	// IdxStride is the record interval between idx entries. 0 →
	// DefaultIdxStride. The header (seq=0) always gets an idx entry.
	IdxStride int

	// FlushInterval is the debounce delay from first dirty write to fsync.
	// 0 → DefaultFlushInterval.
	FlushInterval time.Duration

	// IdleCloseAfter is how long an inactive perKeyWriter keeps its fd.
	// 0 → DefaultIdleCloseAfter.
	IdleCloseAfter time.Duration

	// ChannelBuffer sizes the ingest queue. 0 → DefaultChannelBuffer.
	// Batches arriving when full are dropped (not blocked) and counted.
	ChannelBuffer int

	// Generator is the naozhi build identifier written into each new
	// file's FileHeader.
	Generator string

	// Clock is used for debounce / idle-close / rotate-epoch naming. nil → time.Now.
	Clock func() time.Time

	// DevMode tags the replay-leak slog.Error with dev_mode=true so broken
	// SetPersistSink ordering surfaces in dev/CI logs. Production sets false.
	DevMode bool

	// Observer receives Persister counter increments. nil → noop.
	Observer Observer
}

// Default tuning knobs. DefaultChannelBuffer absorbs a 50-session × 50-batch
// burst (~2500 jobs) without tripping the drop path; a batchJob slot is
// ~120 B so the cap costs ~500 KB worst case (#1336).
const (
	DefaultMaxFileBytes   int64         = 100 * 1024 * 1024 // 100 MiB
	DefaultFlushInterval  time.Duration = 200 * time.Millisecond
	DefaultIdleCloseAfter time.Duration = 10 * time.Minute
	DefaultChannelBuffer                = 4096
)

// Persister owns the single writer goroutine that fans in batches from
// all sessions and serialises them to per-key log + idx files. SinkFor
// returns a closure safe from any goroutine (non-blocking send); only the
// run goroutine touches files or p.writers; Stop closes the channel,
// flushes every writer and waits for run to exit.
type Persister struct {
	opts    Options
	in      chan batchJob
	opCh    chan op
	wg      sync.WaitGroup
	closeCh chan struct{}
	closed  atomic.Bool

	writers map[string]*perKeyWriter

	// dropping tracks stems whose files are mid-removal by the async
	// goroutine spawned in handleOp(opDrop). Batches for such a stem are
	// deferred into dropState.pending (run must never block on a slow
	// unlink) and replayed on opDropDone, so a same-key recreate's O_CREATE
	// lands strictly AFTER the unlink (#1774, #1848). Mutated only on the
	// run goroutine; `done` is closed by the async goroutine. Absent key =
	// no removal in flight.
	dropping map[string]*dropState

	// fs is the filesystem classification captured at startup; never
	// mutated after NewPersister returns.
	fs FSDetection

	// counters exposed for /health + doctor.
	writtenCnt    atomic.Int64
	droppedCnt    atomic.Int64
	fsyncCnt      atomic.Int64
	malformedCnt  atomic.Int64
	replayLeakCnt atomic.Int64

	// lastDrainNS is stamped each time run finishes a batch; WriterAlive reads it.
	lastDrainNS atomic.Int64

	// scratch is the run goroutine's private reuse buffers. Grouping them makes
	// the confinement structural: every field above this one is reachable from
	// other goroutines, everything inside scratch is not.
	scratch flushScratch
}

// batchJob is the internal queue element. Key is the original (un-hashed)
// session key; Entries are schema-marshalled bodies. arena owns the backing
// bytes of every Entry.JSON when accept() copied borrowed bytes (#1524);
// nil when the producer supplied owned bytes (putEntryArena tolerates nil).
type batchJob struct {
	Key     string
	Stem    string
	Entries []Entry
	arena   *batchArena
}

// dropState is the per-stem bookkeeping for an in-flight async unlink:
// the completion channel (closed by the removeKeyFiles goroutine) plus a
// FIFO of batchJobs that arrived mid-drop, each still holding its pooled
// arena until replayed. Mutated only on the run goroutine (#1774, #1848).
type dropState struct {
	done    chan struct{}
	pending []batchJob
}

// droppingPendingMaxBatches caps how many batches one dropping stem may
// defer (each pins an arena + payloads); overflow takes the same drop
// telemetry path as a full channel. A healthy unlink never approaches it.
const droppingPendingMaxBatches = 256

// NewPersister validates opts, ensures Dir exists, sweeps rotate staging
// orphans, and starts the writer goroutine. On error nothing is left
// half-initialised.
func NewPersister(opts Options) (*Persister, error) {
	if opts.Dir == "" {
		return nil, errors.New("persist: Options.Dir is required")
	}
	if opts.MaxFileBytes == 0 {
		opts.MaxFileBytes = DefaultMaxFileBytes
	}
	if opts.IdxStride <= 0 {
		opts.IdxStride = DefaultIdxStride
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = DefaultFlushInterval
	}
	if opts.IdleCloseAfter <= 0 {
		opts.IdleCloseAfter = DefaultIdleCloseAfter
	}
	if opts.ChannelBuffer <= 0 {
		opts.ChannelBuffer = DefaultChannelBuffer
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.Observer == nil {
		opts.Observer = noopObserver{}
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("create events dir %s: %w", opts.Dir, err)
	}
	// MkdirAll applies perm only to directories it creates; a pre-existing
	// dir keeps its mode. The events dir holds prompts/tool output verbatim,
	// so normalise a pre-created 0755/0777 dir to 0700. Log-and-continue
	// (container bind mounts may not be chmod-able); skip symlinks so we
	// never chmod a target outside the dir.
	if info, lerr := os.Lstat(opts.Dir); lerr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			slog.Warn("event log persist: events dir is a symlink; skipping mode normalise",
				"dir", opts.Dir)
		} else if perm := info.Mode().Perm(); perm != 0o700 {
			if cerr := os.Chmod(opts.Dir, 0o700); cerr != nil {
				slog.Warn("event log persist: chmod events dir to 0700 failed",
					"dir", opts.Dir, "had_mode", perm.String(), "err", cerr)
			} else {
				slog.Info("event log persist: corrected events dir mode to 0700",
					"dir", opts.Dir, "had_mode", perm.String())
			}
		}
	} else {
		slog.Warn("event log persist: lstat events dir failed", "dir", opts.Dir, "err", lerr)
	}
	if _, err := SweepOrphans(opts.Dir); err != nil {
		slog.Warn("event log persist: orphan sweep failed", "err", err)
		// Not fatal.
	}

	p := &Persister{
		opts:     opts,
		in:       make(chan batchJob, opts.ChannelBuffer),
		opCh:     make(chan op, 8), // small — drop/flush are rare
		closeCh:  make(chan struct{}),
		writers:  make(map[string]*perKeyWriter),
		dropping: make(map[string]*dropState),
		fs:       DetectFS(opts.Dir),
	}
	if !p.fs.Supported {
		slog.Warn("event log persist: filesystem is not a recommended target",
			"dir", opts.Dir, "fs_type", p.fs.Type, "err", p.fs.Err)
	}
	p.wg.Add(1)
	go p.run()
	return p, nil
}

// FS returns the filesystem classification for the persister's directory,
// frozen at NewPersister time. Safe from any goroutine.
func (p *Persister) FS() FSDetection {
	if p == nil {
		return FSDetection{Type: FSTypeUnknown}
	}
	return p.fs
}

// Pressure reports ingest channel utilisation in [0, 1] (1 = the next
// SinkFor send drops) so producers can back off before OnDrop fires
// (#1057). Advisory: len/cap are read racily but the value is bounded.
// Returns 0 on a nil receiver or closed persister.
func (p *Persister) Pressure() float64 {
	if p == nil || p.closed.Load() {
		return 0
	}
	c := cap(p.in)
	if c == 0 {
		return 0
	}
	return float64(len(p.in)) / float64(c)
}

// Accept reports whether Pressure() < 0.95, for producers that can defer
// (history backfill, low-priority telemetry). Advisory like Pressure;
// false on a nil receiver or closed persister (#1057).
func (p *Persister) Accept() bool {
	if p == nil || p.closed.Load() {
		return false
	}
	return p.Pressure() < 0.95
}

// SinkFor builds a PersistSink closure for a session key. Callers must
// install it via ring.EventLog.SetPersistSink AFTER any InjectHistory
// completes (RFC §3.2.2). After Stop the sink silently drops.
func (p *Persister) SinkFor(key string) PersistSink {
	// A method value on a small struct captures one pointer instead of a
	// three-variable closure environment (#997).
	return (&sessionSink{p: p, key: key, stem: KeyHash(key)}).accept
}

// sessionSink binds (persister, key, stem) for the PersistSink method
// value returned by SinkFor.
func (p *Persister) DropKey(ctx context.Context, key string) error {
	if p.closed.Load() {
		return ErrPersisterClosed
	}
	done := make(chan error, 1)
	stem := KeyHash(key)
	// opCh rather than the batch channel so drops are not coalesced with
	// pending writes.
	select {
	case p.opCh <- op{kind: opDrop, key: key, stem: stem, done: done}:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.closeCh:
		return ErrPersisterClosed
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Flush fsyncs every dirty perKeyWriter now and waits for completion.
func (p *Persister) Flush(ctx context.Context) error {
	if p.closed.Load() {
		return ErrPersisterClosed
	}
	done := make(chan error, 1)
	select {
	case p.opCh <- op{kind: opFlushAll, done: done}:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.closeCh:
		return ErrPersisterClosed
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop makes the writer goroutine drain remaining batches, flush and
// close every file, and exit. Blocks until it returns or ctx is cancelled.
func (p *Persister) Stop(ctx context.Context) error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(p.closeCh)
	waitCh := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(waitCh)
	}()
	select {
	case <-waitCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stats is a snapshot of observability counters for /health and doctor.
type Stats struct {
	Written      int64
	Dropped      int64
	Fsyncs       int64
	Malformed    int64
	ReplayLeak   int64
	ChannelDepth int
	ChannelCap   int
	LastDrainAgo time.Duration

	// FSType / FSSupported report the filesystem backing opts.Dir, cached
	// at NewPersister (a remount needs a restart). RFC §5.4.
	FSType      string
	FSSupported bool
}

func (p *Persister) Stats() Stats {
	var lastAgo time.Duration
	if ns := p.lastDrainNS.Load(); ns > 0 {
		lastAgo = p.opts.Clock().Sub(time.Unix(0, ns))
	}
	return Stats{
		Written:      p.writtenCnt.Load(),
		Dropped:      p.droppedCnt.Load(),
		Fsyncs:       p.fsyncCnt.Load(),
		Malformed:    p.malformedCnt.Load(),
		ReplayLeak:   p.replayLeakCnt.Load(),
		ChannelDepth: len(p.in),
		ChannelCap:   cap(p.in),
		LastDrainAgo: lastAgo,
		FSType:       p.fs.Type,
		FSSupported:  p.fs.Supported,
	}
}

// WriterAlive is the /health.writer_alive signal (RFC §6.3):
//
//	not closed AND (channel is empty-and-not-full OR recent drain)
//
// An idle persister is healthy (naozhi can see zero events for hours);
// the failure surfaced is "queue non-empty AND no drain in 5s".
func (p *Persister) WriterAlive() bool {
	if p.closed.Load() {
		return false
	}
	// Read fields directly rather than via Stats() to avoid an alloc per probe.
	chanCap := cap(p.in)
	if chanCap == 0 {
		return false
	}
	chanDepth := len(p.in)
	notFull := chanDepth*5 < chanCap*4
	if chanDepth == 0 {
		return notFull
	}
	ns := p.lastDrainNS.Load()
	if ns == 0 {
		return false
	}
	lastAgo := p.opts.Clock().Sub(time.Unix(0, ns))
	drainedRecently := lastAgo > 0 && lastAgo < 5*time.Second
	return drainedRecently && notFull
}

// Errors callers can match with errors.Is.
