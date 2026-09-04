package persist

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
	"github.com/naozhi/naozhi/internal/osutil"
)

// recordBufPool reuses the bytes.Buffer schema.MarshalRecordInto writes
// into so handleBatch avoids json's per-call encodeState alloc. Reset
// before Put; capped by recordBufMaxCap on return.
var recordBufPool = sync.Pool{
	New: func() any {
		// 4 KiB covers typical EventEntry JSON.
		buf := bytes.NewBuffer(make([]byte, 0, 4*1024))
		return buf
	},
}

// recordBufMaxCap caps pooled buffer size so one oversize record does not
// pin a large allocation.
const recordBufMaxCap = 64 * 1024

// putRecordBuf returns buf to the pool unless it grew past recordBufMaxCap.
func putRecordBuf(buf *bytes.Buffer) {
	if buf == nil {
		return
	}
	if buf.Cap() > recordBufMaxCap {
		return
	}
	buf.Reset()
	recordBufPool.Put(buf)
}

// arenaSpan is one entry's byte range inside batchArena.buf; used only by
// accept()'s resolve pass.
type arenaSpan struct{ start, end int }

// batchArena bundles the pooled JSON byte buffer with the two scratch
// slices accept() needs per batch: owned (Entry headers, escape into
// batchJob.Entries) and spans (transient byte ranges). All three share
// the arena's lifetime; handleBatch returns it after the write (#1630).
type batchArena struct {
	buf   *bytes.Buffer
	owned []Entry
	spans []arenaSpan
}

// entryArenaPool backs the batch-level copy accept() makes of each
// borrowed Entry.JSON; one arena holds a whole batch and handleBatch
// returns it once written (#1524).
var entryArenaPool = sync.Pool{
	New: func() any {
		return &batchArena{
			buf:   bytes.NewBuffer(make([]byte, 0, 4*1024)),
			owned: make([]Entry, 0, 32),
			spans: make([]arenaSpan, 0, 32),
		}
	},
}

// entryArenaMaxCap caps pooled arena buffer size.
const entryArenaMaxCap = 256 * 1024

// entryArenaSliceMaxCap caps the owned/spans scratch slices so a giant
// batch (e.g. a 500-entry InjectHistory replay) is not pinned in the pool.
const entryArenaSliceMaxCap = 1024

func putEntryArena(a *batchArena) {
	if a == nil {
		return
	}
	if a.buf == nil || a.buf.Cap() > entryArenaMaxCap {
		return
	}
	a.buf.Reset()
	// Clear so persisted payloads are not pinned between batches; oversized
	// scratch goes to GC rather than the pool.
	if cap(a.owned) > entryArenaSliceMaxCap {
		a.owned = nil
	} else {
		clear(a.owned)
		a.owned = a.owned[:0]
	}
	if cap(a.spans) > entryArenaSliceMaxCap {
		a.spans = nil
	} else {
		a.spans = a.spans[:0]
	}
	entryArenaPool.Put(a)
}

// logWriteBufSize is the bufio.Writer capacity per perKeyWriter.logFile.
// 64 KiB matches ReadFramedBody's reader buffer and absorbs a typical
// framed record (1-20 KiB) without spilling to a syscall mid-frame.
const logWriteBufSize = 64 * 1024

// logBufPool reuses *bufio.Writer instances of capacity logWriteBufSize
// across perKeyWriter create/close cycles (#995). Safe because close()
// flushes before releasing and releaseLogBuf rebinds to io.Discard, so a
// pooled instance never references a closed *os.File; the next Reset(file)
// rebinds it and clears the internal err. bufio.Writer never grows.
var logBufPool = sync.Pool{
	New: func() any {
		// Bound to io.Discard; callers Reset(file) before use.
		return bufio.NewWriterSize(io.Discard, logWriteBufSize)
	},
}

// acquireLogBuf returns a pooled *bufio.Writer rebound to file.
func acquireLogBuf(file *os.File) *bufio.Writer {
	bw := logBufPool.Get().(*bufio.Writer)
	bw.Reset(file)
	return bw
}

// releaseLogBuf returns a bufio.Writer to the pool. Callers MUST have
// flushed already — the slot is rebound to io.Discard so a retained
// reference cannot double-write to the original fd.
func releaseLogBuf(bw *bufio.Writer) {
	if bw == nil {
		return
	}
	bw.Reset(io.Discard)
	logBufPool.Put(bw)
}

// Observer receives real-time counter increments from the Persister;
// implementations typically forward to expvar / Prometheus. Methods are
// called from the writer goroutine or the PersistSink closure and MUST be
// non-blocking and thread-safe.
//
// The only production implementation is eventLogMetricsObserver in
// internal/session/eventlog_metrics.go, wired via Options.Observer. A new
// persister site must pass the same instance or metrics silently fall
// through to noopObserver; this cannot be enforced at compile time (#1171).
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

	// flushCands is scratch reused across tickFlush calls. Run-goroutine only.
	flushCands []flushCandidate
	// tickFlushKeys / tickFlushWs are the (key, writer) scratch tickFlush
	// hands to parallelFsync. Run-goroutine only (#1569).
	tickFlushKeys []string
	tickFlushWs   []*perKeyWriter
	// lastFlushCount is how many flushCands slots the last tick populated,
	// so the next tick clears only those (#1406). Run-goroutine only.
	lastFlushCount int
	// flushAllKeys / flushAllWs are the (key, writer) scratch flushAllLocked
	// hands to parallelFsync; kept separate from the tickFlush scratch so
	// the two paths never alias. Run-goroutine only.
	flushAllKeys []string
	flushAllWs   []*perKeyWriter
	// flushAllErrMu serialises firstErr in flushAllLocked's parallelFsync
	// closure; a field rather than a local to avoid the heap escape from
	// capturing a local mutex's address.
	flushAllErrMu sync.Mutex
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
// install it via cli.EventLog.SetPersistSink AFTER any InjectHistory
// completes (RFC §3.2.2). After Stop the sink silently drops.
func (p *Persister) SinkFor(key string) PersistSink {
	// A method value on a small struct captures one pointer instead of a
	// three-variable closure environment (#997).
	return (&sessionSink{p: p, key: key, stem: KeyHash(key)}).accept
}

// sessionSink binds (persister, key, stem) for the PersistSink method
// value returned by SinkFor.
type sessionSink struct {
	p    *Persister
	key  string
	stem string
}

func (s *sessionSink) accept(entries []Entry, replayPhase bool) {
	p := s.p
	if p.closed.Load() {
		return
	}
	if replayPhase {
		p.replayLeakCnt.Add(int64(len(entries)))
		p.opts.Observer.OnReplayLeak(len(entries))
		// Log rather than panic so the ordering bug is observable via
		// replayLeakCnt / OnReplayLeak without crashing the process.
		slog.Error("event log persist: replay-phase entries reached sink",
			"key", s.key, "count", len(entries),
			"dev_mode", p.opts.DevMode)
		return
	}
	if len(entries) == 0 {
		return
	}
	// Entry.JSON and the entries slice are borrowed — the producer may reuse
	// them as soon as accept returns — so copy every body into one pooled
	// arena and materialise our own headers (#1524). Two passes: append all
	// bytes first (the buffer may grow and move), then resolve sub-slices.
	// owned/spans come from the same arena rather than make() (#1630).
	arena := entryArenaPool.Get().(*batchArena)
	n := len(entries)
	owned := arena.owned
	if cap(owned) >= n {
		owned = owned[:n]
	} else {
		owned = make([]Entry, n)
	}
	spans := arena.spans
	if cap(spans) >= n {
		spans = spans[:n]
	} else {
		spans = make([]arenaSpan, n)
	}
	arena.owned = owned
	arena.spans = spans
	for i, e := range entries {
		start := arena.buf.Len()
		arena.buf.Write(e.JSON)
		spans[i] = arenaSpan{start: start, end: arena.buf.Len()}
		owned[i] = Entry{TimeMS: e.TimeMS}
	}
	all := arena.buf.Bytes()
	for i := range owned {
		owned[i].JSON = all[spans[i].start:spans[i].end]
	}
	job := batchJob{Key: s.key, Stem: s.stem, Entries: owned, arena: arena}
	select {
	case p.in <- job:
	default:
		putEntryArena(arena)
		p.droppedCnt.Add(int64(len(entries)))
		p.opts.Observer.OnDrop(len(entries))
		// channel_used distinguishes a wedged writer from an instantaneous
		// burst overrun (#1184).
		slog.Warn("event log persist: channel full; dropping batch",
			"key", s.key, "count", len(entries),
			"channel_used", len(p.in),
			"channel_cap", cap(p.in))
	}
}

// DropKey closes any open writer for key, then removes its log + idx
// files. Safe from any goroutine; waits for the writer goroutine to
// acknowledge the drop.
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
func (p *Persister) dropInMemoryLocked(key string) {
	if w, ok := p.writers[key]; ok {
		if err := w.close(); err != nil {
			slog.Warn("event log persist: close on drop failed", "key", key, "err", err)
		}
		delete(p.writers, key)
	}
}

// removeKeyFiles unlinks the log + idx for stem. Runs on a goroutine
// spawned by handleOp(opDrop) so a slow os.Remove does not stall the
// batch loop; the error is forwarded to DropKey's done channel (#1284).
func (p *Persister) removeKeyFiles(stem string) error {
	logPath := filepath.Join(p.opts.Dir, stem+logExt)
	idxPath := filepath.Join(p.opts.Dir, stem+idxExt)
	var firstErr error
	if err := removeFileHook(logPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		firstErr = fmt.Errorf("remove log: %w", err)
	}
	if err := removeFileHook(idxPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		if firstErr == nil {
			firstErr = fmt.Errorf("remove idx: %w", err)
		}
	}
	return firstErr
}

// removeFileHook is the test seam for a slow/instrumented unlink (#1774).
var removeFileHook = os.Remove

func (p *Persister) flushAllLocked() error {
	// Only dirty writers form the fan-out payload (#1128).
	if len(p.writers) == 0 {
		return nil
	}
	dirtyKeys := p.flushAllKeys[:0]
	dirtyWs := p.flushAllWs[:0]
	for k, w := range p.writers {
		if !w.dirty {
			continue
		}
		dirtyKeys = append(dirtyKeys, k)
		dirtyWs = append(dirtyWs, w)
	}
	p.flushAllKeys = dirtyKeys
	p.flushAllWs = dirtyWs
	if len(dirtyWs) == 0 {
		return nil
	}
	// Parallel flush; same independence argument as shutdownAll. firstErr
	// is recorded under p.flushAllErrMu (a field to avoid a heap escape).
	var firstErr error
	p.parallelFsync(dirtyKeys, dirtyWs, func(k string, w *perKeyWriter) {
		if err := w.flush(p); err != nil {
			p.flushAllErrMu.Lock()
			if firstErr == nil {
				firstErr = fmt.Errorf("flush %s: %w", k, err)
			}
			p.flushAllErrMu.Unlock()
		}
	})
	// Drop writer pointers so closed writers are not pinned until the next Flush.
	clear(dirtyWs)
	return firstErr
}

// flushCandidate gives tickFlush a stable oldest-first flush order:
// sorting by firstDirtyAt bounds worst-case flush latency to N tick
// intervals regardless of map-iteration randomness.
type flushCandidate struct {
	key string
	w   *perKeyWriter
}

func (p *Persister) tickFlush() {
	// Idle deployments hit this every FlushInterval/2; skip the walk + Clock() (#1110).
	if len(p.writers) == 0 {
		return
	}
	now := p.opts.Clock()
	cands := p.collectFlushCandidates(now)
	if len(cands) == 0 {
		return
	}
	// Fan the per-candidate flush (fsync log + idx) over the bounded pool so
	// one slow fsync does not stall every other dirty writer for the tick;
	// candidates are distinct writers, so fn touches no shared state (#1569).
	keys := p.tickFlushKeys[:0]
	ws := p.tickFlushWs[:0]
	for _, c := range cands {
		keys = append(keys, c.key)
		ws = append(ws, c.w)
	}
	p.tickFlushKeys = keys
	p.tickFlushWs = ws
	p.parallelFsync(keys, ws, func(k string, w *perKeyWriter) {
		if err := w.flush(p); err != nil {
			slog.Warn("event log persist: debounced flush failed",
				"key", k, "err", err)
		}
	})
	// Drop writer pointers so closed writers are not pinned until the next tick.
	clear(ws)
}

// collectFlushCandidates returns writers whose firstDirtyAt has aged past
// the adaptive flush interval, sorted oldest-first. Reuses p.flushCands;
// safe because tickFlush is the sole consumer and runs on the run goroutine.
func (p *Persister) collectFlushCandidates(now time.Time) []flushCandidate {
	// Lengthen the window as the writer-set grows to damp the sustained
	// fsync rate (50 sessions × 100 ms tick = 100 fsync/s). Computed once
	// per tick so the bucket is stable across the iteration.
	threshold := effectiveFlushInterval(p.opts.FlushInterval, len(p.writers))
	// Clear only the slots the previous tick populated so closed writers
	// can be GC'd without memzeroing a burst-grown array every tick (#1406).
	if p.lastFlushCount > 0 {
		n := p.lastFlushCount
		if n > cap(p.flushCands) {
			n = cap(p.flushCands)
		}
		clear(p.flushCands[:n])
	}
	cands := p.flushCands[:0]
	for k, w := range p.writers {
		if !w.dirty {
			continue
		}
		if now.Sub(w.firstDirtyAt) < threshold {
			continue
		}
		cands = append(cands, flushCandidate{key: k, w: w})
	}
	if len(cands) > 1 {
		slices.SortFunc(cands, func(a, b flushCandidate) int {
			return a.w.firstDirtyAt.Compare(b.w.firstDirtyAt)
		})
	}
	// Keep the grown backing array; remember how many slots to clear next tick.
	p.flushCands = cands
	p.lastFlushCount = len(cands)
	return cands
}

// effectiveFlushInterval returns the adaptive debounce window used by
// tickFlush. Fixed buckets (≤16 → 1×, ≤64 → 1.5×, ≤256 → 2×, else 4×)
// avoid oscillation as the session count drifts across a boundary. At 50
// writers the window goes 200 → 300 ms, cutting fsync rate ~100/s → ~67/s
// without changing durability (flush still happens, just batching more).
func effectiveFlushInterval(base time.Duration, writerCount int) time.Duration {
	switch {
	case writerCount <= 16:
		return base
	case writerCount <= 64:
		return base + base/2 // 1.5×
	case writerCount <= 256:
		return base * 2
	default:
		return base * 4
	}
}

func (p *Persister) tickIdleClose() {
	if len(p.writers) == 0 {
		return
	}
	now := p.opts.Clock()
	for k, w := range p.writers {
		if w.dirty {
			continue
		}
		if now.Sub(w.lastActivity) < p.opts.IdleCloseAfter {
			continue
		}
		if err := w.close(); err != nil {
			slog.Warn("event log persist: idle close failed",
				"key", k, "err", err)
		}
		delete(p.writers, k)
	}
}

// handleBatch is the hot path: find-or-open the writer, append every
// entry, mark dirty for debounce. It NEVER fsyncs — the debounce ticker
// owns fsync so a 500-entry batch does not cause 500 fsyncs. `now` is
// captured by the caller so one clock read also covers lastDrainNS.
func (p *Persister) handleBatch(job batchJob, now time.Time) {
	// Stem mid-removal: defer into the per-stem FIFO instead of blocking on
	// the unlink. The deferred job keeps its arena (the replaying handleBatch
	// returns it), so return WITHOUT the defer below. Overflow past the cap
	// takes the channel-full drop path (#1848).
	if ds, ok := p.dropping[job.Stem]; ok {
		if len(ds.pending) >= droppingPendingMaxBatches {
			putEntryArena(job.arena)
			n := len(job.Entries)
			p.droppedCnt.Add(int64(n))
			p.opts.Observer.OnDrop(n)
			slog.Warn("event log persist: dropping-stem pending cap reached; dropping batch",
				"key", job.Key, "stem", job.Stem, "count", n,
				"pending", len(ds.pending))
			return
		}
		ds.pending = append(ds.pending, job)
		return
	}

	// Return the arena owning this batch's Entry.JSON once every entry is
	// handled; the defer covers every early return. nil-safe (#1524).
	defer putEntryArena(job.arena)
	w, err := p.writerFor(job.Key, job.Stem)
	if err != nil {
		slog.Error("event log persist: cannot open writer",
			"key", job.Key, "err", err)
		return
	}

	// One pooled buffer for the whole batch amortises json's encodeState alloc.
	encBuf := recordBufPool.Get().(*bytes.Buffer)
	defer putRecordBuf(encBuf)
	var written int
	// One stack Record reused per entry: MarshalRecordInto only reads it
	// synchronously and never retains the pointer (#2088).
	var rec schema.Record
	for _, e := range job.Entries {
		rec.V = schema.WireVersion
		rec.Seq = w.nextSeq
		rec.Type = schema.TypeEntry
		rec.Entry = json.RawMessage(e.JSON)
		encBuf.Reset()
		body, err := schema.MarshalRecordInto(encBuf, &rec)
		if err != nil {
			// Over-size / malformed — count and drop just this entry.
			p.malformedCnt.Add(1)
			p.opts.Observer.OnMalformed()
			slog.Warn("event log persist: marshal entry failed",
				"key", job.Key, "seq", w.nextSeq, "err", err)
			continue
		}
		// Always write through logBuf, never WriteRecordRaw(logFile, ...):
		// bytes written straight to the fd would land out of order relative
		// to anything still pending in the bufio buffer.
		n, err := WriteRecordRaw(w.logBuf, body)
		if err != nil {
			// Drop just this record; WriteRecordRaw writes the whole frame or
			// nothing, so file state stays consistent.
			p.droppedCnt.Add(1)
			p.opts.Observer.OnDrop(1)
			slog.Warn("event log persist: write entry failed",
				"key", job.Key, "seq", w.nextSeq, "err", err)
			continue
		}
		// Pending idx entry — we hold it until fsync time to keep
		// log-before-idx ordering (see recovery.go).
		w.pendingIdx = append(w.pendingIdx, schema.IdxEntry{
			Seq:     w.nextSeq,
			ByteOff: w.bytes,
			Len:     int32(n),
			TimeMS:  e.TimeMS,
		})
		w.bytes += n
		w.nextSeq++
		// entriesSinceIdxWrite is NOT advanced here: it is the stride-cycle
		// phase of pendingIdx[0], read by flush() as selectForIdx's start and
		// advanced by len(pendingIdx) mod stride only after a durable idx
		// sync. Advancing per entry would double-count and break alignment.
		written++
	}
	if written > 0 {
		p.writtenCnt.Add(int64(written))
		p.opts.Observer.OnWrite(written)
	}
	if !w.dirty {
		w.dirty = true
		w.firstDirtyAt = now
	}
	w.lastActivity = now

	// Rotate after the whole batch so its records are never split across
	// old/new files.
	if w.bytes >= p.opts.MaxFileBytes {
		if err := w.flush(p); err != nil {
			slog.Warn("event log persist: pre-rotate flush failed",
				"key", job.Key, "err", err)
		} else if err := p.rotate(job.Key, job.Stem, w); err != nil {
			slog.Warn("event log persist: rotate failed",
				"key", job.Key, "err", err)
		}
	}
}

// writerFor returns an open perKeyWriter for key, creating or
// recovering the file pair on first access.
func (p *Persister) writerFor(key, stem string) (*perKeyWriter, error) {
	if w, ok := p.writers[key]; ok {
		return w, nil
	}

	// handleBatch gates on p.dropping before calling here and opDropDone
	// deletes the entry before replaying, so the stem is never mid-removal
	// at this point (remove-before-recreate, #1774).

	logPath := filepath.Join(p.opts.Dir, stem+logExt)
	idxPath := filepath.Join(p.opts.Dir, stem+idxExt)

	// Recover brings the (log, idx) pair to a consistent state BEFORE
	// opening for append so the first write lands at a known-clean offset.
	rec, err := Recover(logPath, idxPath)
	if err != nil {
		return nil, fmt.Errorf("recover %s: %w", key, err)
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log %s: %w", logPath, err)
	}
	idxW, err := NewIdxWriter(idxPath, 0o600)
	if err != nil {
		logFile.Close()
		return nil, fmt.Errorf("open idx %s: %w", idxPath, err)
	}

	// Pre-size pendingIdx to two stride windows to avoid grow churn.
	pendingCap := 16
	if p.opts.IdxStride > 1 {
		pendingCap = p.opts.IdxStride * 2
	}
	now := p.opts.Clock()
	w := &perKeyWriter{
		key:          key,
		stem:         stem,
		logFile:      logFile,
		logBuf:       acquireLogBuf(logFile),
		idxWriter:    idxW,
		logPath:      logPath,
		idxPath:      idxPath,
		nextSeq:      rec.NextSeq,
		bytes:        rec.LogSize,
		pendingIdx:   make([]schema.IdxEntry, 0, pendingCap),
		lastActivity: now,
	}

	// Fresh file: emit the header at seq=0.
	if rec.LogSize == 0 && !rec.HeaderValid {
		hdr := schema.NewHeader(key, now.UnixMilli(), p.opts.Generator)
		body, mErr := schema.MarshalRecord(hdr)
		if mErr != nil {
			logFile.Close()
			idxW.Close()
			return nil, fmt.Errorf("marshal initial header: %w", mErr)
		}
		n, err := WriteRecordRaw(logFile, body)
		if err != nil {
			logFile.Close()
			idxW.Close()
			return nil, fmt.Errorf("write initial header: %w", err)
		}
		w.pendingIdx = append(w.pendingIdx, schema.IdxEntry{
			Seq: 0, ByteOff: 0, Len: int32(n), TimeMS: hdr.Header.CreatedAt,
		})
		w.bytes = n
		w.nextSeq = 1
		w.dirty = true
		w.firstDirtyAt = now

		// fsync the header now so a crash before any other write leaves a
		// valid file rather than 0 bytes.
		if err := w.flush(p); err != nil {
			logFile.Close()
			idxW.Close()
			return nil, fmt.Errorf("flush initial header: %w", err)
		}
		// SyncDir so the new file's dirent is durable.
		if err := osutil.SyncDir(p.opts.Dir); err != nil {
			slog.Warn("event log persist: SyncDir after header failed",
				"dir", p.opts.Dir, "err", err)
		}
	}

	p.writers[key] = w
	return w, nil
}

// perKeyWriter is per-session state owned exclusively by the writer
// goroutine; no mutex needed.
type perKeyWriter struct {
	key       string
	stem      string
	logFile   *os.File
	logBuf    *bufio.Writer // wraps logFile; flushed before Sync()
	idxWriter *IdxWriter
	logPath   string
	idxPath   string

	nextSeq              uint64
	bytes                int64
	pendingIdx           []schema.IdxEntry // buffered until fsync time
	idxScratch           []schema.IdxEntry // selectForIdx scratch, reused across flushes
	entriesSinceIdxWrite int

	dirty        bool
	firstDirtyAt time.Time
	lastActivity time.Time
}

// flush writes pending idx entries with strict log→idx ordering, fsyncs
// both, and clears dirty. No-op when nothing is dirty.
func (w *perKeyWriter) flush(p *Persister) error {
	if !w.dirty {
		return nil
	}
	// Phase 1: drain bufio, then fsync the log fd. Both must complete before
	// any idx write touches disk (recovery.go: idx must never run ahead of
	// log). On failure dirty stays true so the next tick retries; the bufio
	// Flush error surfaces the original stashed Write failure.
	if err := w.logBuf.Flush(); err != nil {
		return fmt.Errorf("flush log buffer: %w", err)
	}
	if err := w.logFile.Sync(); err != nil {
		return fmt.Errorf("sync log: %w", err)
	}
	p.fsyncCnt.Add(1)
	p.opts.Observer.OnFsync()

	// Phase 2: append pending idx entries, then fsync idx.
	idxAppended := false
	if len(w.pendingIdx) > 0 {
		// Sparse idx: keep the first of every stride, the header (seq=0) and
		// the last entry so recovery finds a safe edge near EOF.
		kept := selectForIdx(w.pendingIdx, p.opts.IdxStride, w.entriesSinceIdxWrite, w.idxScratch[:0])
		// Retain `kept` as scratch only when stride > 1: in the stride<=1 path
		// selectForIdx returns `pending` itself, and aliasing it into idxScratch
		// would let the next flush append into both slices over one backing
		// array, corrupting the idx.
		if p.opts.IdxStride > 1 {
			w.idxScratch = kept
		}
		// AppendBatch consumes `kept` synchronously (idx.go's slice-ownership
		// contract), so the aliasing is safe. If it ever retains `entries`,
		// the stride<=1 path must copy here.
		if err := w.idxWriter.AppendBatch(kept); err != nil {
			return fmt.Errorf("append idx batch: %w", err)
		}
		idxAppended = true
	}
	// Skip the idx fsync when nothing was appended (idx is only valid up to
	// a previously-fsynced suffix anyway). The Sync MUST run BEFORE pendingIdx
	// is discarded and the stride cursor advanced: AppendBatch only reached
	// the page cache, and clearing the retry buffer on a transient Sync error
	// stranded idx bytes that recovery later used to truncate durable log (#1816).
	if idxAppended {
		if err := w.idxWriter.Sync(); err != nil {
			return fmt.Errorf("sync idx: %w", err)
		}
		p.fsyncCnt.Add(1)
		p.opts.Observer.OnFsync()

		// Durability confirmed — safe to discard the retry buffer and advance
		// the stride cursor (mod stride keeps successive batches aligned).
		w.entriesSinceIdxWrite = (w.entriesSinceIdxWrite + len(w.pendingIdx)) % p.opts.IdxStride
		// Shrink if a one-off large batch (e.g. a 500-entry InjectHistory
		// replay) bloated cap far past the steady-state IdxStride*2, else the
		// writer pins the peak capacity for its lifetime. idxScratch grows the
		// same way (#1120) and is only assigned when stride > 1.
		if p.opts.IdxStride > 1 && cap(w.pendingIdx) > p.opts.IdxStride*4 {
			w.pendingIdx = make([]schema.IdxEntry, 0, p.opts.IdxStride*2)
		} else {
			w.pendingIdx = w.pendingIdx[:0]
		}
		if p.opts.IdxStride > 1 && cap(w.idxScratch) > p.opts.IdxStride*4 {
			w.idxScratch = make([]schema.IdxEntry, 0, p.opts.IdxStride*2)
		}
	}

	w.dirty = false
	return nil
}

// close flushes then releases fds; the writer is not reusable afterward.
// The explicit logBuf Flush covers callers that close() without a
// preceding flush() (e.g. shutdownAll after a flush error still wants
// fds released); errors here are best-effort, flush() is where they surface.
func (w *perKeyWriter) close() error {
	var firstErr error
	if w.logBuf != nil {
		if err := w.logBuf.Flush(); err != nil && firstErr == nil {
			firstErr = err
		}
		// releaseLogBuf rebinds the pooled writer to io.Discard so the nilled
		// w.logBuf cannot route writes through it later (#995).
		releaseLogBuf(w.logBuf)
		w.logBuf = nil
	}
	if w.logFile != nil {
		if err := w.logFile.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		w.logFile = nil
	}
	if w.idxWriter != nil {
		if err := w.idxWriter.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		w.idxWriter = nil
	}
	return firstErr
}

// selectForIdx applies the sparse-idx policy: keep the first entry, every
// stride-th entry after it relative to cursor, and always the last entry
// so recovery finds a safe edge within stride-1 records of EOF.
//
// `scratch` is caller-owned (perKeyWriter.idxScratch[:0]) and reused across
// flushes. Aliasing contract: when stride <= 1 or len(pending) == 1 the
// function returns `pending` itself. The sole caller (flush) hands the
// result to idxWriter.AppendBatch, which copies SYNCHRONOUSLY before
// returning, then resets pendingIdx[:0]. Do NOT add an async AppendBatch
// or retain the returned slice without a defensive copy.
func selectForIdx(pending []schema.IdxEntry, stride, cursor int, scratch []schema.IdxEntry) []schema.IdxEntry {
	if stride <= 1 {
		return pending
	}
	if len(pending) == 0 {
		return nil
	}
	// A lone entry is both first and last: always kept, skip the loop.
	if len(pending) == 1 {
		return pending
	}
	estCap := len(pending)/stride + 2
	var kept []schema.IdxEntry
	if cap(scratch) >= estCap {
		kept = scratch[:0]
	} else {
		kept = make([]schema.IdxEntry, 0, estCap)
	}
	for i, e := range pending {
		// Header (seq=0) is always kept.
		if e.Seq == 0 {
			kept = append(kept, e)
			continue
		}
		// Stride-aligned relative to cursor.
		if (cursor+i)%stride == 0 {
			kept = append(kept, e)
			continue
		}
		// Last entry of the batch is always kept.
		if i == len(pending)-1 {
			kept = append(kept, e)
		}
	}
	return kept
}
