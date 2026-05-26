package persist

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
	"github.com/naozhi/naozhi/internal/osutil"
)

// recordBufPool reuses the bytes.Buffer that schema.MarshalRecordInto
// writes into so handleBatch's hot path avoids the encodeState alloc
// json.Marshal performs per call.
//
// R245-PERF-12 [REFACTOR R242-PERF-13]: mirrors the bridgeEncPool
// idiom in internal/session/eventlog_bridge.go. The buffer is borrowed
// for one record at a time; we Reset before Put so the pooled state is
// always clean and we cap with recordBufMaxCap so a one-off oversize
// EventEntry does not pin a multi-MB buffer in the pool.
var recordBufPool = sync.Pool{
	New: func() any {
		// 4 KiB matches typical EventEntry JSON sizes (small assistant
		// content blocks land well under). The buffer grows naturally
		// for larger records and is capped on return.
		buf := bytes.NewBuffer(make([]byte, 0, 4*1024))
		return buf
	},
}

// recordBufMaxCap caps buffer reuse so a one-off oversize record does
// not permanently pin a large heap allocation in the pool.
const recordBufMaxCap = 64 * 1024

// putRecordBuf returns buf to the pool, dropping it if it grew past
// the cap (the next Get will allocate fresh).
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

// logWriteBufSize is the capacity of the bufio.Writer wrapped around
// each perKeyWriter.logFile. 64 KiB matches ReadFramedBody's reader
// buffer and comfortably absorbs typical EventEntry records (1-20
// KiB JSON) plus the length-prefix framing without spilling to a
// syscall mid-frame. The buffer is owned by exactly one goroutine
// so sizing up has no contention cost, only a one-time 64 KiB alloc
// per active session.
const logWriteBufSize = 64 * 1024

// Observer receives real-time counter increments from the Persister.
// Implementations typically forward to expvar / Prometheus; the
// interface keeps the persist package independent of any specific
// metrics library.
//
// All methods are called from the single writer goroutine or from
// the PersistSink closure — implementations MUST be non-blocking
// and thread-safe.
type Observer interface {
	// OnWrite is called once per EventEntry that reaches disk.
	OnWrite(n int)
	// OnDrop is called once per EventEntry dropped because the
	// PersistSink channel was full.
	OnDrop(n int)
	// OnFsync is called each time the persister fsyncs log or idx.
	OnFsync()
	// OnMalformed is called when schema.MarshalRecord rejects an
	// entry (e.g. oversize body).
	OnMalformed()
	// OnReplayLeak is called with the batch size when a batch
	// tagged replayPhase=true reaches the sink (violation of the
	// SetPersistSink-after-InjectHistory contract).
	OnReplayLeak(n int)
}

// noopObserver discards every counter tick. Used when Options.Observer
// is nil (tests, deployments that opt out of metrics).
type noopObserver struct{}

func (noopObserver) OnWrite(int)      {}
func (noopObserver) OnDrop(int)       {}
func (noopObserver) OnFsync()         {}
func (noopObserver) OnMalformed()     {}
func (noopObserver) OnReplayLeak(int) {}

// Options configures a Persister. Defaults apply for zero-valued
// fields so callers only have to set what they want to override.
type Options struct {
	// Dir is the directory <keyhash>.log / <keyhash>.idx files live
	// under. Required; missing dir is an error at NewPersister.
	Dir string

	// MaxFileBytes triggers rotate when a log file grows past this
	// size. 0 → DefaultMaxFileBytes.
	MaxFileBytes int64

	// IdxStride is the record interval between idx entries. 0 →
	// DefaultIdxStride. Record seq=0 (the header) always gets an
	// idx entry regardless.
	IdxStride int

	// FlushInterval is the debounce delay between the first dirty
	// write and the subsequent fsync. 0 → DefaultFlushInterval.
	FlushInterval time.Duration

	// IdleCloseAfter is how long an inactive perKeyWriter holds its
	// fd before the Persister closes it to free the descriptor. 0 →
	// DefaultIdleCloseAfter.
	IdleCloseAfter time.Duration

	// ChannelBuffer sizes the Persister's ingest queue. 0 →
	// DefaultChannelBuffer. Batches that arrive when the channel is
	// full are dropped (not blocked) and counted in dropped_total.
	ChannelBuffer int

	// Generator is the naozhi build identifier written into every
	// new file's FileHeader. Operators reading `jq` output should be
	// able to tell which build produced the file.
	Generator string

	// Clock is used for debounce / idle-close / rotate-epoch naming.
	// Tests inject a manual clock; production leaves this nil and
	// picks up time.Now.
	Clock func() time.Time

	// DevMode tags slog.Error logs with `dev_mode=true` when a batch
	// arrives with replayPhase=true. Used in dev builds + CI so any
	// broken SetPersistSink ordering surfaces immediately in logs.
	// Tests should assert on Observer.OnReplayLeak / replayLeakCnt
	// rather than expecting a panic — see R242-GO-11. Production
	// sets this false.
	DevMode bool

	// Observer receives Persister counter increments. nil → noop.
	// In production the session layer wires an implementation that
	// forwards to internal/metrics expvar counters.
	Observer Observer
}

// Default tuning knobs. See Options godoc for rationale.
const (
	DefaultMaxFileBytes   int64         = 100 * 1024 * 1024 // 100 MiB
	DefaultFlushInterval  time.Duration = 200 * time.Millisecond
	DefaultIdleCloseAfter time.Duration = 10 * time.Minute
	DefaultChannelBuffer                = 1024
)

// Persister owns the single writer goroutine that fan-ins batches
// from all sessions and serialises them to per-key log + idx files.
// Thread model:
//   - SinkFor produces a PersistSink closure callers hook into
//     cli.EventLog. The closure is safe to call from any goroutine
//     (it performs a non-blocking channel send).
//   - One internal goroutine (run) drains the channel and touches
//     files. No other goroutine opens file descriptors.
//   - Stop() closes the channel, flushes all outstanding writers,
//     and returns when the writer goroutine has exited.
type Persister struct {
	opts    Options
	in      chan batchJob
	opCh    chan op
	wg      sync.WaitGroup
	closeCh chan struct{}
	closed  atomic.Bool

	writers map[string]*perKeyWriter

	// fs is the filesystem classification captured at startup. Never
	// mutated after NewPersister returns — exposing a changing value
	// would mislead operators whose only reliable intervention is a
	// restart.
	fs FSDetection

	// counters exposed for /health + doctor.
	writtenCnt    atomic.Int64
	droppedCnt    atomic.Int64
	fsyncCnt      atomic.Int64
	malformedCnt  atomic.Int64
	replayLeakCnt atomic.Int64

	// lastDrainNS updates every time the run goroutine finishes
	// handling a batch. WriterAlive reads this to check liveness.
	lastDrainNS atomic.Int64

	// flushCands is a scratch slice reused across tickFlush calls so the
	// per-tick `var cands []flushCandidate` inside collectFlushCandidates
	// no longer allocates fresh on every flush tick. Touched ONLY on the
	// run goroutine (tickFlush is invoked under the same select that owns
	// p.writers), so no synchronisation is needed. Pre-grown to the
	// 16-writer "no scaling" bucket to cover small deploys in zero
	// allocations; larger writer-counts naturally grow the slice on the
	// first call and stay grown for the process lifetime.
	// R249-PERF-19.
	flushCands []flushCandidate
}

// batchJob is the internal queue element. Key is the original
// (un-hashed) session key. Entries are already schema-marshalled
// bodies pulled from cli.EventEntry upstream.
type batchJob struct {
	Key     string
	Stem    string
	Entries []Entry
}

// NewPersister validates opts, ensures Dir exists, sweeps rotate
// staging orphans, and spins up the background writer. Returns a
// fully ready Persister or an error that is safe to surface to the
// caller (nothing has been left in a half-initialised state).
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
	// R247-SEC-12: MkdirAll honours `perm` only on directories it actually
	// creates — pre-existing components (including opts.Dir itself, when
	// laid down by a prior process or by an attacker who racd ahead of
	// startup) keep whatever mode they had. The events dir holds the
	// session JSONL stream + idx files which carry user prompts and tool
	// outputs verbatim; world-readable parent dirs leak the file names
	// (channel:chatType:id hashes) and any fs.ReadDir-style enumeration
	// surfaces them. Chmod the leaf to the contractual 0o700 so a
	// pre-created 0o755 / 0o777 dir is corrected on the next startup; we
	// log + continue rather than fail because operators sometimes run
	// naozhi inside containers where the bind-mount root cannot be
	// chmod'd by the running uid (NoNewPrivileges, read-only rootfs)
	// and a hard fail there would brick the persister entirely. The Lstat
	// guard keeps us from following a symlink to a location we should not
	// be modifying — if the path is a symlink, log the surprise and skip
	// the chmod (the SweepOrphans call below will flag oddly-shaped
	// states for the operator anyway).
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
	// Sweep rotate stagings from any prior crashed session.
	if _, err := SweepOrphans(opts.Dir); err != nil {
		slog.Warn("event log persist: orphan sweep failed", "err", err)
		// Not fatal — swept or not, regular operation can start.
	}

	p := &Persister{
		opts:    opts,
		in:      make(chan batchJob, opts.ChannelBuffer),
		opCh:    make(chan op, 8), // small — drop/flush are rare
		closeCh: make(chan struct{}),
		writers: make(map[string]*perKeyWriter),
		fs:      DetectFS(opts.Dir),
	}
	if !p.fs.Supported {
		// Operators deserve a prominent log line; doctor + /health
		// surface the same signal persistently afterwards.
		slog.Warn("event log persist: filesystem is not a recommended target",
			"dir", opts.Dir, "fs_type", p.fs.Type, "err", p.fs.Err)
	}
	p.wg.Add(1)
	go p.run()
	return p, nil
}

// FS returns the cached filesystem classification for the persister's
// directory. Safe to call from any goroutine — the value is frozen
// at NewPersister time.
func (p *Persister) FS() FSDetection {
	if p == nil {
		return FSDetection{Type: FSTypeUnknown}
	}
	return p.fs
}

// SinkFor builds a PersistSink closure for a specific session key.
// Callers (session.Router.spawnSession) pass the returned closure to
// cli.EventLog.SetPersistSink AFTER any InjectHistory completes — see
// RFC §3.2.2. Safe to call before Stop; after Stop the sink silently
// drops (it is a caller bug to send through a stopped persister, but
// dropping is the least surprising behaviour).
func (p *Persister) SinkFor(key string) PersistSink {
	stem := KeyHash(key)
	return func(entries []Entry, replayPhase bool) {
		if p.closed.Load() {
			return
		}
		if replayPhase {
			p.replayLeakCnt.Add(int64(len(entries)))
			p.opts.Observer.OnReplayLeak(len(entries))
			// R242-GO-11 [BREAKING-LOCAL] (closed): previously this branch
			// panic'd in DevMode to make sink-ordering bugs explode loudly
			// during tests; the panic was a goroutine-context crash that
			// took down the whole process and could not be observed cleanly
			// by callers. We now log at Error level with a `dev_mode=...`
			// attribute and rely on the `replayLeakCnt` counter +
			// `OnReplayLeak` Observer hook (both already in place above)
			// for test assertions and prod alerting. See
			// TestPersister_DevMode_ReplayLeakObserved for the contract pin.
			slog.Error("event log persist: replay-phase entries reached sink",
				"key", key, "count", len(entries),
				"dev_mode", p.opts.DevMode)
			return
		}
		if len(entries) == 0 {
			return
		}
		job := batchJob{Key: key, Stem: stem, Entries: entries}
		select {
		case p.in <- job:
		default:
			p.droppedCnt.Add(int64(len(entries)))
			p.opts.Observer.OnDrop(len(entries))
			slog.Warn("event log persist: channel full; dropping batch",
				"key", key, "count", len(entries),
				"channel_cap", cap(p.in))
		}
	}
}

// DropKey closes any open writer for key, then removes its log + idx
// files. Safe to call from any goroutine; synchronously waits for
// the writer goroutine to acknowledge the drop. Used by
// session.Router.ResetChat / Remove / Cleanup.
func (p *Persister) DropKey(ctx context.Context, key string) error {
	if p.closed.Load() {
		return ErrPersisterClosed
	}
	done := make(chan error, 1)
	job := batchJob{Key: key, Stem: KeyHash(key), Entries: nil /* drop signal */}
	// Use the pass-through op channel instead of the batch channel so
	// drops don't get coalesced with pending writes. Implemented as a
	// dedicated method on the writer goroutine via opCh below.
	select {
	case p.opCh <- op{kind: opDrop, key: key, stem: job.Stem, done: done}:
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

// Flush forces every perKeyWriter's debounce timer to fire
// immediately and waits for the pending fsyncs to complete. Exposed
// for tests and for the router's Shutdown hook.
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

// Stop signals the writer goroutine to drain remaining batches,
// flush all open files, close fds, and exit. Blocks until the
// goroutine returns or ctx is cancelled.
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

// Stats returns a snapshot of observability counters. Used by
// /health.eventlog and doctor.
type Stats struct {
	Written      int64
	Dropped      int64
	Fsyncs       int64
	Malformed    int64
	ReplayLeak   int64
	ChannelDepth int
	ChannelCap   int
	LastDrainAgo time.Duration

	// FSType / FSSupported report the detected filesystem backing
	// Persister.opts.Dir. Cached at NewPersister time (syscall is
	// cheap but operators don't expect the mount to change under
	// their feet mid-run; if they remount, a service restart picks
	// it up). FSSupported==false surfaces as a banner on dashboard
	// / doctor per RFC §5.4.
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

// WriterAlive is the /health.writer_alive signal. See RFC §6.3.
//
// Healthy persister = worker accepting and draining work. An idle
// persister (no sessions producing events) is NOT unhealthy, so the
// signal is:
//
//	not closed AND (channel is empty-and-not-full OR recent drain)
//
// The empty-channel shortcut covers cold-start + long idle windows
// (naozhi can legitimately see zero events for hours). The recent-
// drain branch catches "queue has work and worker is progressing".
// The failure mode we actually want to surface is "queue non-empty
// AND no drain in 5s" — i.e. a stalled writer.
func (p *Persister) WriterAlive() bool {
	if p.closed.Load() {
		return false
	}
	// Read fields directly instead of via Stats() to avoid the
	// 80-byte struct allocation per /health probe. R250-PERF-24.
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
)

type op struct {
	kind opKind
	key  string
	stem string
	done chan error
}

// run is the single writer goroutine's main loop. It multiplexes
// batch writes from `in` and control operations from `opCh`, plus a
// debounce ticker and a low-frequency idle sweeper. Holding this
// structure in ONE goroutine simplifies concurrency drastically —
// no locks are needed on p.writers or any perKeyWriter field.
func (p *Persister) run() {
	defer p.wg.Done()

	// Debounce ticker: checks every FlushInterval whether any writer
	// has a pending fsync due. Granularity = FlushInterval/2 so an
	// event written just after a tick waits ~1.5× FlushInterval worst
	// case, ~0.5× best — well within our stated 200 ms contract.
	flushTick := p.opts.FlushInterval / 2
	if flushTick < 10*time.Millisecond {
		flushTick = 10 * time.Millisecond
	}
	flushT := time.NewTicker(flushTick)
	defer flushT.Stop()

	// Idle sweeper: runs every IdleCloseAfter/4. Cheap scan over the
	// writer map closing any fd that hasn't been written to recently.
	idleTick := p.opts.IdleCloseAfter / 4
	if idleTick < 30*time.Second {
		idleTick = 30 * time.Second
	}
	idleT := time.NewTicker(idleTick)
	defer idleT.Stop()

	for {
		select {
		case job := <-p.in:
			// Capture Clock() once per batch and reuse for both the
			// dirty-flag/lastActivity bookkeeping inside handleBatch and
			// the lastDrainNS atomic store below. Saves one vDSO call per
			// hot-path batch (~5-50/s steady state). R222-PERF-12.
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
			// R222-GO-6: drain pending DropKey/Flush ops with
			// ErrPersisterClosed so the caller's `<-done` arm completes
			// promptly instead of waiting for ctx to fire. A caller that
			// already passed the closed.Load() guard but raced into opCh
			// before close(closeCh) lands here; without this drain the
			// caller would block until ctx (often a 30s shutdown budget)
			// expires. Non-blocking: opCh is buffered=8, so a bounded
			// drain loop is sufficient.
			for {
				select {
				case o := <-p.opCh:
					if o.done != nil {
						// Buffered (cap=1) so this never blocks.
						o.done <- ErrPersisterClosed
					}
				default:
					p.shutdownAll()
					return
				}
			}
		}
	}
}

// shutdownAll closes every writer, fsyncing first so we don't lose a
// debounce window's worth of data on a clean Stop.
func (p *Persister) shutdownAll() {
	for k, w := range p.writers {
		if err := w.flush(p); err != nil {
			slog.Warn("event log persist: flush on shutdown failed",
				"key", k, "err", err)
		}
		if err := w.close(); err != nil {
			slog.Warn("event log persist: close on shutdown failed",
				"key", k, "err", err)
		}
		delete(p.writers, k)
	}
}

func (p *Persister) handleOp(o op) {
	var err error
	switch o.kind {
	case opDrop:
		// Drop must observe all prior writes for this key, otherwise a
		// "send then DropKey" sequence can race: the in-flight batch
		// would arrive AFTER the remove and recreate the files. Drain
		// the in channel first.
		p.drainInChannel()
		err = p.dropLocked(o.key, o.stem)
	case opFlushAll:
		// Same rationale as opDrop: Flush must observe every pending
		// batchJob before fsyncing. Without this, tests and /health
		// callers get a Flush return before their recent writes are
		// durable.
		p.drainInChannel()
		err = p.flushAllLocked()
	}
	if o.done != nil {
		o.done <- err
	}
}

// drainInChannel pulls every queued batchJob off p.in and writes it,
// blocking until the channel is empty. Only the run goroutine calls
// this (via handleOp), so there's no concurrent competition for the
// recv side.
func (p *Persister) drainInChannel() {
	// One Clock+atomic Store after the loop instead of one per batch:
	// drain may pull dozens of queued batches in a tight burst.
	// R216-PERF-7. Each handleBatch reuses the same captured `now` so a
	// burst of 50 batches needs only one vDSO call. R222-PERF-12.
	var now time.Time
	drained := false
	for {
		select {
		case job := <-p.in:
			if !drained {
				now = p.opts.Clock()
			}
			p.handleBatch(job, now)
			drained = true
		default:
			if drained {
				p.lastDrainNS.Store(now.UnixNano())
			}
			return
		}
	}
}

func (p *Persister) dropLocked(key, stem string) error {
	if w, ok := p.writers[key]; ok {
		_ = w.close()
		delete(p.writers, key)
	}
	logPath := filepath.Join(p.opts.Dir, stem+logExt)
	idxPath := filepath.Join(p.opts.Dir, stem+idxExt)
	var firstErr error
	if err := os.Remove(logPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		firstErr = fmt.Errorf("remove log: %w", err)
	}
	if err := os.Remove(idxPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		if firstErr == nil {
			firstErr = fmt.Errorf("remove idx: %w", err)
		}
	}
	return firstErr
}

func (p *Persister) flushAllLocked() error {
	var firstErr error
	for k, w := range p.writers {
		if err := w.flush(p); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("flush %s: %w", k, err)
			}
		}
	}
	return firstErr
}

// flushCandidate is reused inside tickFlush for a stable oldest-first
// flush order across map iterations. Without this Go's randomised map
// iter occasionally starves a writer that crossed the FlushInterval
// boundary by re-entering tickFlush before fsync runs (the run loop's
// fsync gate is global; one slow fsync can monopolise a tick window).
// Sorting by firstDirtyAt makes the worst-case flush latency bounded
// to N tick intervals, regardless of map iter randomness. R247-PERF-26.
type flushCandidate struct {
	key string
	w   *perKeyWriter
}

func (p *Persister) tickFlush() {
	// R250-PERF-6 (#1110): mirror the empty-writer guard already in
	// tickIdleClose. Idle deployments (cron-only / dashboard-paused) hit
	// this every FlushInterval/2 (≥10ms) and the empty-map walk + Clock
	// vDSO call are pure overhead — at the default 100ms cadence that
	// is ~864k context switches/day waking just to confirm zero writers.
	// Safe without locking because tickFlush runs only on the run
	// goroutine, the same goroutine that mutates p.writers.
	if len(p.writers) == 0 {
		return
	}
	now := p.opts.Clock()
	// Collect-then-sort instead of a true heap: 1-200 typical writers
	// per tick, sort.Slice is faster in practice than a container/heap
	// init+pop loop at that N. The slice itself is allocated once per
	// tick — see flushCandidatePool below if profiling later indicates
	// this matters.
	cands := p.collectFlushCandidates(now)
	if len(cands) == 0 {
		return
	}
	for _, c := range cands {
		if err := c.w.flush(p); err != nil {
			slog.Warn("event log persist: debounced flush failed",
				"key", c.key, "err", err)
		}
	}
}

// collectFlushCandidates returns writers whose firstDirtyAt has aged
// past the (adaptive) flush interval, sorted oldest-first. Split out so
// tickFlush stays readable and the sort + adaptive scaling can be
// unit-tested independently.
//
// R249-PERF-19: reuses p.flushCands rather than allocating a fresh
// slice each tick. The clear+truncate is safe because tickFlush is
// the sole reader of the returned slice (no goroutine retains a
// reference past the loop) and runs only on the run goroutine.
func (p *Persister) collectFlushCandidates(now time.Time) []flushCandidate {
	// R214-PERF-3: lengthen the effective flush interval as the live
	// writer-set grows. Each writer's flush() does its own fsync(log) +
	// fsync(idx); 50 sessions × 100 ms tick = 100 fsync/s sustained,
	// dominating disk IO on slow SSD-backed instances. Scaling the
	// debounce window damps the per-second fsync rate proportionally
	// while preserving the FlushInterval semantics (worst-case latency
	// is still bounded by the scaled interval). Computed once per tick
	// rather than per writer so the bucket boundary is stable across
	// the iteration.
	threshold := effectiveFlushInterval(p.opts.FlushInterval, len(p.writers))
	// Drop pointer references from the previous tick before reuse so
	// dropped/idle-closed writers can be GC'd while this slice holds
	// the only remaining reference; then truncate to len=0 keeping cap.
	clear(p.flushCands)
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
		sort.Slice(cands, func(i, j int) bool {
			return cands[i].w.firstDirtyAt.Before(cands[j].w.firstDirtyAt)
		})
	}
	// Stash the (possibly grown) backing array back so the next tick
	// inherits the larger cap.
	p.flushCands = cands
	return cands
}

// effectiveFlushInterval returns the adaptive debounce window applied
// inside tickFlush. The base is FlushInterval; the multiplier grows in
// fixed buckets so the resulting window doesn't oscillate as session
// count drifts across a single boundary.
//
// Buckets (writerCount):
//
//	≤16  → 1.0× (default, single-tab dashboards / unit tests)
//	17–64 → 1.5× (typical small deploys)
//	65–256 → 2.0× (busy production hosts)
//	>256  → 4.0× (cap; running this many concurrent sessions on one
//	             host already implies operator opt-in to longer flush
//	             windows)
//
// At 50 writers (the issue's headline scenario) the window goes from
// 200 ms → 300 ms, cutting fsync rate from ~100/s to ~67/s without
// changing durability semantics — flush still happens; it just batches
// more entries between syncs.
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
	// R249-PERF-20: skip the Clock() vDSO call + map iter setup when no
	// writers are open. Idle deployments (cron-only / dashboard-paused)
	// hit this every IdleCloseAfter/4 (≥30s) and the empty-map walk is
	// pure overhead. Safe without locking because tickIdleClose runs
	// only on the run goroutine, the same goroutine that mutates
	// p.writers — no other goroutine can append between this read and
	// the loop body.
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
// entry, update the dirty flag for debounce. It NEVER fsyncs here;
// the debounce ticker owns fsync so a 500-entry batch doesn't cause
// 500 fsyncs.
//
// `now` is captured by the caller (run loop or drainInChannel) so the
// same clock reading covers both this function's bookkeeping and the
// caller's lastDrainNS update. R222-PERF-12.
func (p *Persister) handleBatch(job batchJob, now time.Time) {
	w, err := p.writerFor(job.Key, job.Stem)
	if err != nil {
		slog.Error("event log persist: cannot open writer",
			"key", job.Key, "err", err)
		return
	}

	// R245-PERF-12: borrow a single pooled buffer for every record in this
	// batch so the encodeState alloc inside encoding/json is amortised
	// across the batch rather than paid per-entry. The buffer is reset
	// between records via Truncate(0) (cheap; just resets the read/write
	// cursor on the underlying slice).
	encBuf := recordBufPool.Get().(*bytes.Buffer)
	defer putRecordBuf(encBuf)
	for _, e := range job.Entries {
		rec := schema.NewEntry(w.nextSeq, e.JSON)
		encBuf.Reset()
		body, err := schema.MarshalRecordInto(encBuf, rec)
		if err != nil {
			// Over-size / malformed — count and drop just this entry.
			p.malformedCnt.Add(1)
			p.opts.Observer.OnMalformed()
			slog.Warn("event log persist: marshal entry failed",
				"key", job.Key, "seq", w.nextSeq, "err", err)
			continue
		}
		// logBuf wraps logFile; flush() Flushes it before Sync().
		// Never call WriteRecordRaw(logFile, ...) directly here —
		// bytes written straight to the fd would bypass logBuf and
		// land out of order relative to anything still pending in
		// the bufio buffer.
		n, err := WriteRecordRaw(w.logBuf, body)
		if err != nil {
			// Write failures on individual records are treated as
			// "drop the record" — the whole file state is preserved
			// (WriteRecordRaw either wrote all bytes of the frame or
			// none), and we continue with the rest of the batch.
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
		w.entriesSinceIdxWrite++
		p.writtenCnt.Add(1)
		p.opts.Observer.OnWrite(1)
	}
	if !w.dirty {
		w.dirty = true
		w.firstDirtyAt = now
	}
	w.lastActivity = now

	// Rotate gate: check after batch so we don't split a batch's
	// records across old/new files mid-way. If the rotate triggers it
	// writes everything pending, renames, then the next batch starts
	// the new file.
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

	logPath := filepath.Join(p.opts.Dir, stem+logExt)
	idxPath := filepath.Join(p.opts.Dir, stem+idxExt)

	// Recover brings the (log, idx) pair into a consistent state
	// BEFORE we open them for append. This guarantees the first
	// post-open write lands at a known-clean offset.
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

	// Pre-cap pendingIdx so the typical batch fills without triggering
	// nil→1→2→4→… grow churn. IdxStride drives the steady-state fill
	// (one append per IdxStride events), so 2*IdxStride covers two
	// stride windows comfortably. R218-PERF-8.
	pendingCap := 16
	if p.opts.IdxStride > 1 {
		pendingCap = p.opts.IdxStride * 2
	}
	w := &perKeyWriter{
		key:          key,
		stem:         stem,
		logFile:      logFile,
		logBuf:       bufio.NewWriterSize(logFile, logWriteBufSize),
		idxWriter:    idxW,
		logPath:      logPath,
		idxPath:      idxPath,
		nextSeq:      rec.NextSeq,
		bytes:        rec.LogSize,
		pendingIdx:   make([]schema.IdxEntry, 0, pendingCap),
		lastActivity: p.opts.Clock(),
	}

	// Emit a header if this is a fresh file (log empty after
	// recovery). Header goes at seq=0.
	if rec.LogSize == 0 && !rec.HeaderValid {
		hdr := schema.NewHeader(key, p.opts.Clock().UnixMilli(), p.opts.Generator)
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
		w.firstDirtyAt = p.opts.Clock()

		// Directly fsync the header so a crash before any other
		// write leaves a valid file rather than 0 bytes. Cheap (one
		// fsync per new session).
		if err := w.flush(p); err != nil {
			logFile.Close()
			idxW.Close()
			return nil, fmt.Errorf("flush initial header: %w", err)
		}
		// SyncDir once so the new file is guaranteed visible.
		if err := osutil.SyncDir(p.opts.Dir); err != nil {
			slog.Warn("event log persist: SyncDir after header failed",
				"dir", p.opts.Dir, "err", err)
		}
	}

	p.writers[key] = w
	return w, nil
}

// perKeyWriter holds the per-session mutable state the writer
// goroutine touches exclusively. No mutex needed because the goroutine
// is the sole owner.
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

// flush writes pending idx entries (with strict log→idx ordering),
// fsyncs both, and resets the dirty flag. Safe to call when nothing
// is dirty — becomes a no-op.
func (w *perKeyWriter) flush(p *Persister) error {
	if !w.dirty {
		return nil
	}
	// Phase 1: drain the bufio buffer, then fsync the backing fd.
	// Both must complete before any idx write touches disk (see
	// recovery.go's idx-ahead-of-log reasoning). A failure at either
	// step aborts the flush; dirty stays true so the next tick
	// retries. The bufio.Flush error surfaces the original Write
	// failure that got stashed in bufio's internal err field.
	if err := w.logBuf.Flush(); err != nil {
		return fmt.Errorf("flush log buffer: %w", err)
	}
	if err := w.logFile.Sync(); err != nil {
		return fmt.Errorf("sync log: %w", err)
	}
	p.fsyncCnt.Add(1)
	p.opts.Observer.OnFsync()

	// Phase 2: write all pending idx entries (already buffered —
	// no work to serialise bytes) and fsync idx.
	idxAppended := false
	if len(w.pendingIdx) > 0 {
		// Apply stride: the first entry of every N is sparse-written,
		// plus header (seq=0) and the last entry of the batch (so
		// recovery can always find a safe edge near EOF). Dropping
		// middle entries is the reason idx is sparse.
		kept := selectForIdx(w.pendingIdx, p.opts.IdxStride, w.entriesSinceIdxWrite, w.idxScratch[:0])
		// Only retain `kept` as the persistent scratch when stride > 1 — in
		// the stride<=1 fast path selectForIdx returns `pending` itself, and
		// aliasing it into idxScratch would let the next flush share the
		// backing array with pendingIdx (pendingIdx[:0] keeps the same
		// array). On reuse, append into both slices would corrupt the idx.
		if p.opts.IdxStride > 1 {
			w.idxScratch = kept
		}
		// R243-PERF-10: AppendBatch consumes `kept` synchronously (see
		// idx.go's slice-ownership contract). The aliasing is therefore
		// safe — by the time we reset pendingIdx[:0] below, every byte of
		// `kept` has already been marshalled into idxWriter.batchBuf and
		// flushed to the underlying *os.File. If AppendBatch ever changes
		// to retain `entries` (e.g. async write, deferred coalescing) the
		// stride<=1 path must defensively copy here OR force kept != pending
		// unconditionally.
		if err := w.idxWriter.AppendBatch(kept); err != nil {
			return fmt.Errorf("append idx batch: %w", err)
		}
		idxAppended = true
		// entriesSinceIdxWrite is a cursor into the stride cycle —
		// reset modulo stride so successive batches stay aligned.
		w.entriesSinceIdxWrite = (w.entriesSinceIdxWrite + len(w.pendingIdx)) % p.opts.IdxStride
		// Shrink the backing array if a one-off large batch (e.g.
		// InjectHistory replay of 500+ entries) bloated cap far
		// beyond the steady-state IdxStride*2 sizing. Without this
		// guard the per-session writer permanently retains the
		// peak capacity once an oversized batch has flowed through.
		if p.opts.IdxStride > 1 && cap(w.pendingIdx) > p.opts.IdxStride*4 {
			w.pendingIdx = make([]schema.IdxEntry, 0, p.opts.IdxStride*2)
		} else {
			w.pendingIdx = w.pendingIdx[:0]
		}
	}
	// Skip the fsync entirely when this flush did not append any idx
	// bytes — under high session count + short FlushInterval the idx
	// fsync runs every tick whether or not new data landed, doubling
	// disk fsync pressure for nothing. Recovery is unaffected: idx is
	// only valid up to a previously-fsynced suffix anyway.
	if idxAppended {
		if err := w.idxWriter.Sync(); err != nil {
			return fmt.Errorf("sync idx: %w", err)
		}
		p.fsyncCnt.Add(1)
		p.opts.Observer.OnFsync()
	}

	w.dirty = false
	return nil
}

// close flushes then releases fds. After close the writer is not
// reusable — callers discard the instance.
//
// Flush semantics: a clean close drains logBuf via flush(); rotate
// calls close() AFTER its own flush() has already drained the bufio
// (see handleBatch's pre-rotate flush). The explicit Flush here
// covers callers that invoke close() without a preceding flush —
// notably shutdownAll's per-writer iteration where a flush error
// would still leave us wanting to release fds. We ignore the Flush
// error because Close() is already about best-effort cleanup; the
// preceding flush() path is where errors should have surfaced.
func (w *perKeyWriter) close() error {
	var firstErr error
	if w.logBuf != nil {
		if err := w.logBuf.Flush(); err != nil && firstErr == nil {
			firstErr = err
		}
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

// selectForIdx applies the sparse-idx policy: keep the first entry,
// every stride-th entry after it relative to the cursor, and always
// keep the last entry in the batch. Keeping the last entry means
// recovery can locate the nearest safe edge within stride-1 bytes of
// EOF rather than up to stride records back.
//
// `scratch` is a caller-owned slice (typically perKeyWriter.idxScratch[:0])
// that is reused across flushes to avoid a per-flush heap allocation. When
// `stride <= 1` the function returns `pending` directly without touching
// scratch. R218-PERF-7.
//
// R243-PERF-10: aliasing contract. When the function returns `pending`
// (stride<=1 fast path or single-entry batch), the returned slice
// shares its backing array with `pending`. The sole caller in flush()
// passes the returned slice straight to idxWriter.AppendBatch, which
// SYNCHRONOUSLY copies every entry into its own batchBuf and Writes it
// before returning — see idx.go:76. After AppendBatch returns the
// caller resets `pendingIdx[:0]` and the alias is severed before any
// subsequent append into pendingIdx can touch the slot. Do NOT add an
// async / queued AppendBatch variant or expose the returned slice to
// callers that may retain it: the next pendingIdx append would
// overwrite live idx bytes the writer is still reading. If a future
// caller cannot honour synchronous consumption it must take a
// defensive copy (`append([]schema.IdxEntry(nil), kept...)`).
func selectForIdx(pending []schema.IdxEntry, stride, cursor int, scratch []schema.IdxEntry) []schema.IdxEntry {
	if stride <= 1 {
		return pending
	}
	if len(pending) == 0 {
		return nil
	}
	// Single-entry batch: that lone entry is simultaneously the first and the
	// last entry of the batch, so the kept-policy below would always retain
	// it. Skip the scratch slice + loop allocation. R240-PERF-6.
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
