package persist

import (
	"bufio"
	"bytes"
	"context"
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

// arenaSpan records one entry's byte range inside batchArena.buf. Held
// only for the second resolve pass in accept(); never escapes the batch.
type arenaSpan struct{ start, end int }

// batchArena bundles the pooled JSON byte buffer with the two scratch
// slices accept() needs per batch: `owned` (the materialised Entry
// headers, which escape into batchJob.Entries) and `spans` (the
// transient per-entry byte ranges). Folding owned/spans into the same
// pooled object as the buffer eliminates the two per-batch slice-header
// allocs accept() previously paid (R20260602-PERF-7, #1630) without
// introducing a second global pool: the slices share the arena's
// lifetime exactly (handleBatch returns the whole batchArena once the
// batch's Entry.JSON bytes are written and no longer referenced).
type batchArena struct {
	buf   *bytes.Buffer
	owned []Entry
	spans []arenaSpan
}

// entryArenaPool backs the batch-level copy accept() makes of each
// borrowed Entry.JSON (R20260531A-PERF-3, #1524). One arena holds every
// entry's bytes for a single batch; handleBatch returns it after the
// batch is written. Pooling the arena replaces the bridge's former
// per-event make([]byte, N) with amortised reuse, eliminating that
// steady-state heap churn (50 events/s × N sessions).
var entryArenaPool = sync.Pool{
	New: func() any {
		return &batchArena{
			buf:   bytes.NewBuffer(make([]byte, 0, 4*1024)),
			owned: make([]Entry, 0, 32),
			spans: make([]arenaSpan, 0, 32),
		}
	},
}

// entryArenaMaxCap caps arena reuse so a one-off giant batch does not
// pin a large heap allocation in the pool.
const entryArenaMaxCap = 256 * 1024

// entryArenaSliceMaxCap caps the owned/spans scratch slices so a one-off
// giant batch (e.g. a 500-entry InjectHistory replay leaking through)
// does not pin oversized slice backing arrays in the pool for the
// process lifetime. Sized generously above the typical 50-event batch.
const entryArenaSliceMaxCap = 1024

func putEntryArena(a *batchArena) {
	if a == nil {
		return
	}
	if a.buf == nil || a.buf.Cap() > entryArenaMaxCap {
		// Buffer missing or grown too large: drop the whole arena so the
		// pool never hands back a giant backing array.
		return
	}
	a.buf.Reset()
	// Drop entry-pointer references so the persisted EventEntry payloads
	// are not pinned by the pooled scratch between batches, then reset
	// length keeping capacity for reuse. Oversized scratch slices are
	// released to GC (set to nil) rather than pooled.
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

// logWriteBufSize is the capacity of the bufio.Writer wrapped around
// each perKeyWriter.logFile. 64 KiB matches ReadFramedBody's reader
// buffer and comfortably absorbs typical EventEntry records (1-20
// KiB JSON) plus the length-prefix framing without spilling to a
// syscall mid-frame. The buffer is owned by exactly one goroutine
// so sizing up has no contention cost, only a one-time 64 KiB alloc
// per active session.
const logWriteBufSize = 64 * 1024

// logBufPool reuses *bufio.Writer instances of capacity logWriteBufSize
// across perKeyWriter create / close cycles. R249-PERF-21 (#995): without
// pooling, every fresh writer (initial NewPersister attach, idle TTL
// evict + respawn, post-rotate reattach) paid a 64 KiB heap allocation.
// On a busy deployment with N sessions cycling through the writer cache
// that is N × 64 KiB of churn the GC has to reclaim per cycle.
//
// The pool is safe because perKeyWriter.close() flushes before nilling
// w.logBuf, and we Reset(io.Discard) on the way out so the pooled
// instance carries no reference to the closed *os.File. The next
// Reset(logFile) call on Get rebinds it to the fresh fd, clearing the
// internal err field at the same time. Returning to the pool a writer
// that grew past logWriteBufSize is impossible — bufio.Writer never
// grows its buffer; capacity is fixed by NewWriterSize.
var logBufPool = sync.Pool{
	New: func() any {
		// Bind to io.Discard initially; callers Reset(file) before use.
		// Using Discard at construction means the pool fast-path on the
		// very first Get of the program lifetime returns a usable
		// writer rather than the New func returning nil and forcing
		// an extra branch at the call site.
		return bufio.NewWriterSize(io.Discard, logWriteBufSize)
	},
}

// acquireLogBuf returns a *bufio.Writer rebound to file. The returned
// writer's internal buffer capacity is exactly logWriteBufSize.
func acquireLogBuf(file *os.File) *bufio.Writer {
	bw := logBufPool.Get().(*bufio.Writer)
	bw.Reset(file)
	return bw
}

// releaseLogBuf returns a flushed bufio.Writer to the pool. Callers MUST
// have already called Flush (or close()'s flush) — the pool slot is
// rebound to io.Discard so any retained reference cannot accidentally
// double-write to the original fd through the pooled writer.
func releaseLogBuf(bw *bufio.Writer) {
	if bw == nil {
		return
	}
	bw.Reset(io.Discard)
	logBufPool.Put(bw)
}

// Observer receives real-time counter increments from the Persister.
// Implementations typically forward to expvar / Prometheus; the
// interface keeps the persist package independent of any specific
// metrics library.
//
// All methods are called from the single writer goroutine or from
// the PersistSink closure — implementations MUST be non-blocking
// and thread-safe.
//
// Wiring contract (R250-ARCH-8 / #1171): the Observer interface is
// defined here so persist has zero dependency on the metrics layer, but
// the **only** production implementation lives in
// internal/session/eventlog_metrics.go (eventLogMetricsObserver), wired
// in by session.NewRouter via Options.Observer. Adding a new persister
// site (e.g. /api/admin/eventlog or a planner-local persister) MUST
// either pass the same eventLogMetricsObserver instance or accept that
// metrics will silently fall through to noopObserver — the persist
// package cannot enforce that wiring at compile time. The
// TestPersister_ObserverWiring_OnWriteOnFsync contract test pins the
// "OnWrite is called when an entry reaches disk and OnFsync is called
// during Flush" invariant so a future Observer-method addition that
// breaks the metrics path surfaces immediately rather than during a
// production drop investigation.
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
//
// R20260527122801-PERF-5 (#1336 partial): raised DefaultChannelBuffer
// from 1024 → 4096. The original 1024 cap was sized for ~20 active
// sessions; in steady-state with 50+ concurrent dashboards each
// flushing 50-event batches the burst arrival could near-saturate the
// channel and trip the drop telemetry path (handleBatch → droppedCnt).
// 4× headroom absorbs the 50×50 burst (~2500 batches) without
// architectural change. The full fan-out fix proposed in #1336
// (N writer goroutines via KeyHash mod N) is preserved as a follow-up
// when the singleton drain rate itself becomes the bottleneck — at
// 4096 cap that's ~10× further out than the prior cap allowed.
//
// Memory cost: each batchJob is ~120 B (slice header + key + 50 entry
// pointers); 4096 slots × 120 B ≈ 500 KB worst-case in-flight, which
// is dominated by the EventEntry payloads referenced from the slots
// (those exist with or without buffering). Raising the cap therefore
// only buys headroom for the queue depth, not new resident memory.
const (
	DefaultMaxFileBytes   int64         = 100 * 1024 * 1024 // 100 MiB
	DefaultFlushInterval  time.Duration = 200 * time.Millisecond
	DefaultIdleCloseAfter time.Duration = 10 * time.Minute
	DefaultChannelBuffer                = 4096
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

	// dropping tracks stems whose on-disk files are mid-removal by the
	// async goroutine spawned in handleOp(opDrop). The per-stem `done`
	// channel is closed by that goroutine once removeKeyFiles returns; the
	// run goroutine's opDropDone case then replays any deferred batches and
	// deletes the entry. This guarantees a same-key recreation's O_CREATE
	// lands strictly AFTER the unlink — otherwise a slow os.Remove could
	// race the recreated file and delete it (#1774).
	//
	// R20260606 (#1848): handleBatch must NOT block the single writer
	// goroutine on the per-stem channel while an unlink is in flight (a slow
	// FUSE/NFS os.Remove would stall every other session's persistence and
	// drop their batches). Instead, batches that arrive for a dropping stem
	// are deferred into dropState.pending and replayed in arrival order on
	// opDropDone, after the unlink completes — preserving the
	// remove-before-recreate invariant without ever blocking run. The map is
	// mutated ONLY on the run goroutine (insert in handleOp, pending append
	// in handleBatch, replay+delete in opDropDone); the `done` channel is
	// closed by the async goroutine. Nil-safe: an absent key means no
	// removal is in flight.
	dropping map[string]*dropState

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
	// tickFlushKeys / tickFlushWs are the parallel (key, writer) scratch
	// slices tickFlush hands to parallelFsync, reused across ticks to keep
	// the fan-out allocation-free in steady state. Run-goroutine only —
	// tickFlush is the sole reader/writer, same as flushCands. R20260602-
	// 091302-PERF-3 (#1569).
	tickFlushKeys []string
	tickFlushWs   []*perKeyWriter
	// lastFlushCount is the number of slots in flushCands that were
	// actually populated on the most recent tick. R040034-PERF-7 (#1406):
	// the prior `clear(p.flushCands)` zeroed every slot up to len, so a
	// one-off burst that grew the slice to N kept memzeroing N pointers
	// every 100 ms tick even when the current candidate count was 0.
	// Tracking the last-used length lets us clear just the slots we
	// actually touched. Run-goroutine only — no synchronisation needed.
	lastFlushCount int
	// flushAllKeys / flushAllWs are the parallel (key, writer) scratch
	// slices flushAllLocked hands to parallelFsync, reused across calls
	// to eliminate the two make() allocations per opFlushAll. Run-goroutine
	// only — flushAllLocked is called exclusively via the opFlushAll case in
	// handleOp, which is driven by the same run goroutine as tickFlush.
	// Kept separate from tickFlushKeys/tickFlushWs: while both run on the
	// same goroutine, flushAllLocked may be triggered independently of the
	// tick path (e.g. explicit Flush calls), so conservative independent
	// fields avoid any cross-path aliasing. R20260603-PERF-2.
	flushAllKeys []string
	flushAllWs   []*perKeyWriter
	// flushAllErrMu serialises firstErr updates inside the parallelFsync
	// closure used by flushAllLocked. Promoted from a local variable to
	// eliminate the heap escape caused by the closure capturing its address.
	// Safe to share without additional coordination: flushAllLocked is only
	// ever called while the run goroutine holds the op-loop (i.e. no two
	// flushAllLocked calls can be concurrent). [R20260603-PERF-17]
	flushAllErrMu sync.Mutex
}

// batchJob is the internal queue element. Key is the original
// (un-hashed) session key. Entries are already schema-marshalled
// bodies pulled from cli.EventEntry upstream.
//
// arena (R20260531A-PERF-3, #1524) is an optional pooled buffer that
// owns the backing bytes for every Entry.JSON in this batch. When the
// producer hands over borrowed bytes (the bridge no longer copies — see
// PersistSink contract), accept() copies them into a pooled arena and
// stores it here so handleBatch can return it to entryArenaPool once the
// batch is durably written. nil when the producer supplied owned bytes
// (e.g. older callers / tests); handleBatch's putEntryArena tolerates nil.
type batchJob struct {
	Key     string
	Stem    string
	Entries []Entry
	arena   *batchArena
}

// dropState is the per-stem bookkeeping for an in-flight async unlink
// (#1774, #1848). It pairs the completion channel (closed by the
// removeKeyFiles goroutine) with a FIFO of batchJobs that arrived for the
// stem while the unlink was still running.
//
// R20260606 (#1848): replaces the bare `chan struct{}` so handleBatch can
// defer (rather than block run on) batches landing mid-drop. The pending
// jobs retain their pooled arena — putEntryArena is deferred until the job
// is replayed by opDropDone (or dropped/flushed). Mutated ONLY on the run
// goroutine; the channel is closed by the async unlink goroutine.
type dropState struct {
	done    chan struct{}
	pending []batchJob
}

// droppingPendingMaxBatches caps how many batches a single dropping stem may
// defer before handleBatch falls back to the drop telemetry path. A slow
// FUSE/NFS unlink should not let an unbounded number of in-flight batches
// (each pinning a pooled arena + EventEntry payloads) accumulate in the
// pending FIFO and exhaust memory; the cap mirrors the channel-full drop
// behaviour (count + Observer.OnDrop + return the arena) so overflow is
// observable rather than silent. Sized generously: a healthy unlink
// completes in microseconds, so reaching this cap means the filesystem is
// pathologically slow and dropping is the least-bad outcome.
const droppingPendingMaxBatches = 256

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
		opts:     opts,
		in:       make(chan batchJob, opts.ChannelBuffer),
		opCh:     make(chan op, 8), // small — drop/flush are rare
		closeCh:  make(chan struct{}),
		writers:  make(map[string]*perKeyWriter),
		dropping: make(map[string]*dropState),
		fs:       DetectFS(opts.Dir),
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

// Pressure reports the current ingest channel utilisation as a value in
// [0, 1] where 0 means "drained" and 1 means "full, next SinkFor send will
// drop". Callers (session router, /health, future planners) can probe this
// to back off proactively instead of discovering backpressure post-hoc via
// the OnDrop counter.
//
// R244-ARCH-2 (#1057): the PersistSink closure used to be the only signal
// path, and OnDrop fires only AFTER an entry is already lost. Pressure() is
// safe to call from any goroutine — len(chan)/cap(chan) are racy by design
// but the value is advisory and bounded, so a torn read at most under- or
// over-states utilisation by one slot.
//
// Returns 0 on a nil receiver or a closed persister so callers do not
// special-case those states (a stopped persister exerts zero ingest
// pressure on the producer and freshly-handed-out closures may already be
// in flight).
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

// Accept reports whether the next SinkFor send would have non-trivial
// capacity. Returns true when Pressure() < 0.95 — i.e. there is at least a
// 5% slot margin remaining. Producers that can defer (e.g. cold-start
// history backfill, low-priority telemetry) should consult Accept() and
// retry later rather than racing into the OnDrop path.
//
// R244-ARCH-2 (#1057): paired with Pressure() so callers without a
// floating-point comparison threshold (e.g. dashboard health badges) can
// branch on a boolean without having to open-code the 0.95 constant.
//
// Like Pressure, Accept is advisory: a true result does not guarantee the
// subsequent send will not race against another producer to fill the
// remaining slack. False on a nil receiver or closed persister so callers
// uniformly treat unavailable backends as "do not produce".
func (p *Persister) Accept() bool {
	if p == nil || p.closed.Load() {
		return false
	}
	return p.Pressure() < 0.95
}

// SinkFor builds a PersistSink closure for a specific session key.
// Callers (session.Router.spawnSession) pass the returned closure to
// cli.EventLog.SetPersistSink AFTER any InjectHistory completes — see
// RFC §3.2.2. Safe to call before Stop; after Stop the sink silently
// drops (it is a caller bug to send through a stopped persister, but
// dropping is the least surprising behaviour).
func (p *Persister) SinkFor(key string) PersistSink {
	// R249-PERF-29 (#997): bind (persister, key, stem) onto a single struct
	// and return its method value rather than a func literal that captures
	// three free variables (p, key, stem). The method-value form captures
	// exactly one receiver pointer, so the per-key sink allocation is a small
	// fixed-size sessionSink (string header + string header + pointer) rather
	// than a multi-variable closure environment, and the binding intent is
	// explicit at the type level.
	return (&sessionSink{p: p, key: key, stem: KeyHash(key)}).accept
}

// sessionSink is the per-session binding for a PersistSink. accept implements
// the PersistSink signature via a method value returned from SinkFor.
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
			"key", s.key, "count", len(entries),
			"dev_mode", p.opts.DevMode)
		return
	}
	if len(entries) == 0 {
		return
	}
	// R20260531A-PERF-3 (#1524): the PersistSink contract now hands us
	// borrowed bytes — the producer (bridge) may reuse Entry.JSON's
	// backing array the moment accept returns. Because the batch is
	// retained across the async channel, we take ownership here by
	// copying every entry's JSON into a single pooled arena. The entries
	// slice header is likewise borrowed, so we materialise our own.
	//
	// Two passes: first append all bytes (the arena may grow and move its
	// backing array), then resolve each Entry.JSON sub-slice once the
	// arena is final. Resolving slices during the append pass would alias
	// a stale backing array after a grow.
	//
	// R20260602-PERF-7 (#1630): owned/spans are borrowed from the pooled
	// batchArena (paired with arena.buf) instead of make()'d fresh per
	// batch, eliminating two slice-header allocs on the hot path. accept
	// is non-reentrant per arena instance (each Get hands out a distinct
	// arena, returned only after handleBatch finishes), so the borrowed
	// slices cannot be aliased across batches. We reslice to the needed
	// length, growing the backing array only when a batch exceeds the
	// pooled capacity.
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
		// R250-ARCH-23 (#1184): include channel_used so operators
		// can distinguish "writer goroutine wedged with N pending
		// jobs" from "instantaneous burst overrun". Without this
		// signal the drop log line tells you which key got starved
		// but not how saturated the queue was at the moment the
		// drop fired — the single most useful piece of context for
		// diagnosing whether the writer is making progress at all.
		// Full per-key fairness (drop additional batches from the
		// same chatty key first) needs a per-key counter map and
		// is a follow-up; the observable signal here unblocks
		// operator triage today.
		slog.Warn("event log persist: channel full; dropping batch",
			"key", s.key, "count", len(entries),
			"channel_used", len(p.in),
			"channel_cap", cap(p.in))
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
	// R250-PERF-18 (#1121): the drop signal travels through opCh, which only
	// needs the hashed stem (key + done are carried as separate op fields).
	// Computing the stem into a local avoids materialising a throwaway
	// batchJob whose Key/Entries fields were never read on this path.
	stem := KeyHash(key)
	// Use the pass-through op channel instead of the batch channel so
	// drops don't get coalesced with pending writes. Implemented as a
	// dedicated method on the writer goroutine via opCh below.
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
	// opDropDone is posted by the async removeKeyFiles goroutine once the
	// unlink completes, so the run goroutine can clear the stem from
	// p.dropping. Carries the same channel that was stored in the map so
	// the delete only fires when it still matches (a fresh opDrop for the
	// same stem may have replaced the entry in between). #1774.
	opDropDone
)

type op struct {
	kind opKind
	key  string
	stem string
	done chan error
	// ch is the per-stem completion channel, set only on opDropDone so
	// the run goroutine can match-and-delete the dropping entry.
	ch chan struct{}
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
					if o.kind == opDropDone {
						// A still-pending unlink finished during shutdown:
						// replay its deferred batches (writerFor will recreate
						// the file) so a clean Stop does not silently lose the
						// in-flight events. Match-and-delete like the live path.
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
					// R20260606 (#1848): replay any batches still deferred
					// behind an in-flight unlink whose opDropDone never landed
					// (the async goroutine took the <-p.closeCh branch). The
					// stem's files are being removed concurrently, but
					// recreating + writing them on a clean Stop preserves the
					// events; if the unlink races ahead the recreate simply
					// lands a fresh file, same as the live drop-then-recreate
					// path. This also returns each deferred job's pooled arena
					// via handleBatch's defer so Stop leaves no arena pinned.
					p.replayDroppingPending()
					p.shutdownAll()
					return
				}
			}
		}
	}
}

// replayDroppingPending drains every dropState.pending FIFO into handleBatch
// on the run goroutine during Stop's shutdown drain, so batches deferred
// behind an in-flight unlink (#1848) are persisted (and their pooled arenas
// returned) rather than silently lost. Called once, after p.in/opCh are
// drained and before shutdownAll. Run-goroutine only — no synchronisation
// needed; the entries are removed from p.dropping as they replay so a
// late-arriving opDropDone for the same stem becomes a no-op.
func (p *Persister) replayDroppingPending() {
	if len(p.dropping) == 0 {
		return
	}
	// Snapshot the pending FIFOs first, then delete every dropping entry, so
	// the subsequent handleBatch replays open/write the recreated files
	// instead of re-deferring into the same dropState (handleBatch gates on
	// p.dropping[stem]). A late opDropDone for any of these stems then finds
	// an absent entry and is a harmless no-op. The map is discarded with the
	// Persister after run returns regardless.
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

// shutdownAll closes every writer, fsyncing first so we don't lose a
// debounce window's worth of data on a clean Stop.
//
// R040034-PERF-13 (#1408): the per-writer flush+close pair issues two
// fsyncs (log + idx) — at 5-20 ms each on a slow SSD, 100+ writers ×
// 2 fsyncs serialised would block Stop for 1-4 s. Parallelise across a
// small worker pool so the shutdown-budget caller observes the slowest
// writer's pair instead of the sum. Each *perKeyWriter is independent
// (distinct fds, distinct buffers; no aliasing through Persister), and
// the only Persister state mutated inside flush()/close() is the
// fsyncCnt atomic + the thread-safe Observer (godoc on Observer
// requires non-blocking + thread-safe). Writers live on the run
// goroutine's owned p.writers map — we iterate to a slice first so
// fan-out doesn't touch the map concurrently with the parallel work.
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

// parallelFsyncMaxWorkers caps the worker-pool size used by
// shutdownAll / flushAllLocked. Each worker may sit blocked in fsync
// for tens of ms on slow disks, so a small pool is intentional —
// 8-way parallelism reduces the ~2N fsync wall time to ~N/8 with a
// bounded number of in-flight kernel writeback paths. Auto-sized
// down to len(writers) when fewer writers are present so a single-
// writer flush stays serial (no WaitGroup overhead).
const parallelFsyncMaxWorkers = 8

// parallelFsyncWorkers is the writable hook tests use to pin a
// single worker for deterministic ordering, or 0 to use the default
// auto-sizing up to parallelFsyncMaxWorkers. Production callers
// leave it at 0.
var parallelFsyncWorkers = 0

// parallelFsync fans `fn` over (keys[i], ws[i]) using a bounded
// worker pool. Inputs of 1-2 writers skip the WaitGroup entirely so
// the typical "1-2 active sessions on Stop" footprint stays cheap.
// Each fn call runs concurrently with at most workers-1 others; the
// caller MUST guarantee that fn does not mutate any state shared
// across writer indices (per-writer fields are safe; persister-
// global state must already be atomic / mutex-guarded — see godoc
// on shutdownAll for the audit). Joins all workers before returning
// so the caller can safely mutate the writers map afterward.
func (p *Persister) parallelFsync(keys []string, ws []*perKeyWriter, fn func(string, *perKeyWriter)) {
	n := len(ws)
	if n == 0 {
		return
	}
	if n <= 2 {
		// R20260607-PERF-4: small deployments (1-2 active writers) flush
		// serially. The goroutine + WaitGroup + atomic setup costs more
		// than running two fsyncs back-to-back, and on the ~100ms
		// tickFlush this overhead recurs steadily. Semantics match the
		// worker-pool path: fn returns nothing and each index is invoked
		// exactly once, so serial vs parallel are equivalent here.
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
		// Drop must observe all prior writes for this key, otherwise a
		// "send then DropKey" sequence can race: the in-flight batch
		// would arrive AFTER the remove and recreate the files. Drain
		// the in channel first.
		p.drainInChannel()
		// R20260527-PERF-4 (#1284): split the drop into a synchronous
		// in-memory phase (close writer fd + delete map entry) and an
		// asynchronous file-removal phase. On slow filesystems
		// (FUSE/NFS) the os.Remove pair could stall the writer
		// goroutine for 100s of ms, blocking every concurrent Append
		// draining p.in (cap DefaultChannelBuffer; drops with telemetry
		// once full). Once the
		// map entry is gone any concurrent SinkFor reaches the empty
		// map and proceeds normally; the unlinks racing the next
		// possible recreation are handled by the same key-stem
		// invariant the synchronous version relied on (KeyHash is
		// deterministic, so a recreated session would reopen log/idx
		// at the same paths regardless of whether the prior unlinks
		// have completed yet — Linux/Darwin allow O_CREAT after
		// unlink-in-flight, the new inode just replaces the dirent).
		p.dropInMemoryLocked(o.key)
		// R20260605 (#1774): publish a per-stem dropState BEFORE spawning the
		// async unlink. The async goroutine closes ds.done right after
		// removeKeyFiles returns and posts opDropDone so the run goroutine can
		// replay any deferred batches and clear the map entry.
		//
		// R20260606 (#1848): handleBatch no longer blocks run on ds.done while
		// the unlink is in flight — instead it defers batches for this stem
		// into ds.pending, which opDropDone replays in order once the unlink
		// completes. The remove-before-recreate invariant is preserved because
		// no writerFor / O_CREATE for this stem runs until opDropDone (and the
		// replay it drives) has observed the closed channel.
		ds := &dropState{done: make(chan struct{})}
		p.dropping[o.stem] = ds
		go func(stem string, done chan error, ch chan struct{}) {
			err := p.removeKeyFiles(stem)
			// Signal completion before notifying the caller, then post
			// opDropDone so the run goroutine replays deferred batches and
			// clears the entry. Carry ch so the replay/delete only fires if
			// this exact dropState is still installed (a newer opDrop for the
			// same stem may have replaced it).
			close(ch)
			select {
			case p.opCh <- op{kind: opDropDone, stem: stem, ch: ch}:
			case <-p.closeCh:
				// Persister shutting down — the map is goroutine-local to
				// run, which has stopped consuming opCh; Stop's drain replays
				// (or releases) any deferred batches.
			}
			if done != nil {
				done <- err
			}
		}(o.stem, o.done, ds.done)
		return // explicit: skip the post-switch o.done write below
	case opDropDone:
		// Replay deferred batches and clear the dropping entry only if it
		// still matches the channel we closed — a fresh opDrop for the same
		// stem may have installed a new dropState in between, which a later
		// opDropDone will retire.
		cur, ok := p.dropping[o.stem]
		if !ok || cur.done != o.ch {
			return
		}
		// Delete BEFORE replaying so the replayed batches' writerFor sees an
		// absent dropping entry and opens the recreated file normally instead
		// of re-deferring into the same (now retired) dropState.
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
		// Same rationale as opDrop: Flush must observe every pending
		// batchJob before fsyncing. Without this, tests and /health
		// callers get a Flush return before their recent writes are
		// durable.
		p.drainInChannel()
		err := p.flushAllLocked()
		if o.done != nil {
			o.done <- err
		}
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
	//
	// R20260531A-PERF-9 (#1525): a single Clock() read for the whole
	// drain would stamp every writer's lastActivity with the pre-drain
	// instant. A long burst (50+ batches) can take long enough that a
	// writer touched late in the drain looks idle to tickIdleClose and
	// gets closed prematurely. Cap that staleness by refreshing `now`
	// every drainClockRefreshEvery batches — still far cheaper than one
	// Clock() per batch, but bounds the lastActivity lag.
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

// drainClockRefreshEvery bounds how stale the `now` captured in
// drainInChannel may become: every N batches the drain re-reads the
// clock so a long burst cannot stamp late writers with the pre-drain
// instant and trip tickIdleClose. 16 keeps the vDSO call rate ~16x
// lower than per-batch while capping staleness to a handful of batches'
// worth of work. R20260531A-PERF-9 (#1525).
const drainClockRefreshEvery = 16

// dropInMemoryLocked closes the per-key writer (if open) and removes its
// map entry. Runs on the writer goroutine — must NOT touch the
// filesystem beyond w.close()'s fd close, since the whole point of
// R20260527-PERF-4 (#1284) is to keep this op fast even when the
// underlying FS is slow on os.Remove.
func (p *Persister) dropInMemoryLocked(key string) {
	if w, ok := p.writers[key]; ok {
		if err := w.close(); err != nil {
			slog.Warn("event log persist: close on drop failed", "key", key, "err", err)
		}
		delete(p.writers, key)
	}
}

// removeKeyFiles unlinks the on-disk log + idx for `stem`. Runs on a
// dedicated goroutine spawned by handleOp(opDrop) so a slow os.Remove
// on FUSE/NFS does not stall the writer's batch loop. R20260527-PERF-4
// (#1284). Errors are returned (and forwarded to the caller's `done`
// channel) preserving the original DropKey synchronous-feeling
// contract — the caller still observes a single return value, just one
// that the caller doesn't have to wait on the writer goroutine for.
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

// removeFileHook is the writable seam tests use to inject a slow or
// instrumented unlink (e.g. to exercise the DropKey-then-recreate race
// guarded by p.dropping, #1774). Production callers leave it at os.Remove.
var removeFileHook = os.Remove

func (p *Persister) flushAllLocked() error {
	// R250-PERF-25 (#1128): pre-filter dirty writers so the fan-out
	// payload is only the actual fsync workload. The dirty bit is
	// only mutated on the run goroutine that owns this iteration so
	// no synchronisation is needed for the read.
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
	// R040034-PERF-13 (#1408): parallel-flush dirty writers via a
	// bounded worker pool. Same independence-of-state argument as
	// shutdownAll — see godoc above for the audit. firstErr is
	// recorded under p.flushAllErrMu so concurrent workers racing to
	// record the first error surface a single representative error.
	// p.flushAllErrMu is a Persister field (see struct) to avoid the
	// heap escape caused by a local sync.Mutex being captured by address
	// in the closure. [R20260603-PERF-17]
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
	// Drop writer-pointer references so dropped/idle-closed writers can be
	// GC'd rather than pinned by the scratch slice until the next Flush.
	// Keys are plain strings; no GC concern, left as-is. Mirrors tickFlush.
	clear(dirtyWs)
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
	// per tick, slices.SortFunc is faster in practice than a container/heap
	// init+pop loop at that N, and avoids the closure-boxing alloc that
	// sort.Slice causes. The slice itself is allocated once per
	// tick — see flushCandidatePool below if profiling later indicates
	// this matters.
	cands := p.collectFlushCandidates(now)
	if len(cands) == 0 {
		return
	}
	// R20260602-091302-PERF-3 (#1569): fan the per-candidate flush() (each
	// of which does fsync(log)+fsync(idx)) over the same bounded worker
	// pool flushAllLocked/shutdownAll use, instead of a serial loop. In the
	// 50+ concurrent-session steady state a single slow fsync no longer
	// stalls every other dirty writer's persistence for the whole tick;
	// wall time drops from ~2N fsyncs serialised to ~2N/workers. The
	// candidates are distinct writers, so fn touches no shared per-writer
	// state — same independence audit as shutdownAll's godoc. parallelFsync
	// keeps the 1-candidate fast path inline (no goroutine spawn), so the
	// common single-tab case is unchanged.
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
	// Drop the writer-pointer references so a writer dropped/idle-closed
	// before the next tick can be GC'd rather than pinned by this scratch
	// slice. Keys are plain strings; no GC concern, left as-is.
	clear(ws)
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
	//
	// R040034-PERF-7 (#1406): only zero the slots the previous tick
	// actually populated (lastFlushCount), not the full backing array.
	// Without this, after a one-off burst grew flushCands to cap=N,
	// every subsequent FlushInterval/2 (~100ms) tick memzeros N pointer
	// slots even when the current candidate count is 0. Bounded by the
	// slice header so a shrunk slice can never index past its own len.
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
	// Stash the (possibly grown) backing array back so the next tick
	// inherits the larger cap. Remember how many slots we just used
	// so the next tick can clear exactly that many — not the full cap.
	p.flushCands = cands
	p.lastFlushCount = len(cands)
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
	// R20260606 (#1848): if this stem is mid-removal, defer the batch into
	// the per-stem pending FIFO instead of blocking the single writer
	// goroutine until the (possibly slow FUSE/NFS) unlink finishes. The
	// deferred batch retains its pooled arena — putEntryArena is the
	// replaying handleBatch's responsibility on opDropDone — so we return
	// WITHOUT the defer below. A cap guards against an unbounded pending
	// FIFO when the unlink is pathologically slow: overflow falls back to
	// the drop telemetry path (count + Observer + arena return), mirroring
	// the channel-full behaviour.
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

	// R20260531A-PERF-3 (#1524): return the pooled arena that owns this
	// batch's Entry.JSON bytes once we're done with every entry. The
	// defer covers the writerFor-error early return below and every
	// continue/return inside the loop. nil-safe for owned-bytes callers.
	defer putEntryArena(job.arena)
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
	var written int
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
		// NOTE: entriesSinceIdxWrite is NOT advanced here. It is a cursor
		// into the stride cycle that records the absolute-stream phase of
		// pendingIdx[0] (the first entry staged since the last successful
		// idx append). flush() reads it as the selectForIdx start phase and
		// then advances it by len(pendingIdx) modulo stride after a durable
		// idx sync. Incrementing per entry here would double-count the batch
		// (cursor advances by 2N per cycle instead of N) and offset the
		// selectForIdx phase by N, breaking the stride alignment invariant.
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

	// R20260606 (#1848): writerFor no longer blocks the run goroutine on a
	// mid-removal stem. handleBatch defers any batch for a dropping stem into
	// dropState.pending and opDropDone replays it only AFTER deleting the
	// dropping entry, so by the time writerFor runs for a recreated session
	// the stem is guaranteed absent from p.dropping. The remove-before-
	// recreate invariant (#1774) therefore still holds without the blocking
	// receive that previously stalled every other session on a slow unlink.
	// (handleBatch is the sole caller and gates on p.dropping before reaching
	// here; writerFor stays defensive by not assuming a dropping entry.)

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
	// R20260603-PERF-13: capture clock once; reuse for lastActivity and
	// hdr.CreatedAt to avoid two vDSO calls when creating a fresh file.
	now := p.opts.Clock()
	w := &perKeyWriter{
		key:          key,
		stem:         stem,
		logFile:      logFile,
		logBuf:       acquireLogBuf(logFile), // R249-PERF-21 (#995): pool 64 KiB bufio.
		idxWriter:    idxW,
		logPath:      logPath,
		idxPath:      idxPath,
		nextSeq:      rec.NextSeq,
		bytes:        rec.LogSize,
		pendingIdx:   make([]schema.IdxEntry, 0, pendingCap),
		lastActivity: now,
	}

	// Emit a header if this is a fresh file (log empty after
	// recovery). Header goes at seq=0.
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
	}
	// Skip the fsync entirely when this flush did not append any idx
	// bytes — under high session count + short FlushInterval the idx
	// fsync runs every tick whether or not new data landed, doubling
	// disk fsync pressure for nothing. Recovery is unaffected: idx is
	// only valid up to a previously-fsynced suffix anyway.
	//
	// R20260605B-CORR-12 (#1816): the idx durability barrier (Sync) MUST
	// run BEFORE we discard the in-memory retry buffer (pendingIdx) and
	// advance the stride cursor. AppendBatch above only wrote idx bytes
	// into the page cache (idx.go's plain Write) — they are NOT durable
	// until Sync() returns nil. The old code reset pendingIdx to empty and
	// advanced entriesSinceIdxWrite first; on a transient idx fsync error
	// flush returned with dirty=true but pendingIdx already empty. The
	// retry flush then appended nothing (idxAppended=false), never
	// re-fsynced idx, and cleared dirty — leaving the just-written idx
	// bytes stranded in page cache. A crash before kernel writeback drove
	// recovery to truncate log records that were already fsynced in
	// Phase 1 (recovery.go idx-behind-log branch), destroying durable
	// data. Syncing first keeps pendingIdx intact on Sync failure so the
	// next flush re-appends and re-fsyncs the same entries idempotently.
	if idxAppended {
		if err := w.idxWriter.Sync(); err != nil {
			return fmt.Errorf("sync idx: %w", err)
		}
		p.fsyncCnt.Add(1)
		p.opts.Observer.OnFsync()

		// Durability confirmed — only now is it safe to discard the
		// retry buffer and advance the stride cursor.
		//
		// entriesSinceIdxWrite is a cursor into the stride cycle —
		// reset modulo stride so successive batches stay aligned.
		w.entriesSinceIdxWrite = (w.entriesSinceIdxWrite + len(w.pendingIdx)) % p.opts.IdxStride
		// Shrink the backing array if a one-off large batch (e.g.
		// InjectHistory replay of 500+ entries) bloated cap far
		// beyond the steady-state IdxStride*2 sizing. Without this
		// guard the per-session writer permanently retains the
		// peak capacity once an oversized batch has flowed through.
		//
		// R250-PERF-17 (#1120): idxScratch tracks the same growth pattern
		// (selectForIdx writes kept entries into the caller-supplied
		// scratch slice), so apply the matching shrink rule here. Without
		// this, after a single InjectHistory burst every per-key writer
		// pins a multi-KB scratch backing array for its lifetime; with
		// 100+ active writers that's avoidable steady-state heap. The
		// stride>1 gate matches the pendingIdx branch above — in the
		// stride<=1 fast path idxScratch is never assigned (selectForIdx
		// returns `pending` itself; see line 1102 guard) so there's
		// nothing to shrink.
		if p.opts.IdxStride > 1 && cap(w.pendingIdx) > p.opts.IdxStride*4 {
			w.pendingIdx = make([]schema.IdxEntry, 0, p.opts.IdxStride*2)
		} else {
			w.pendingIdx = w.pendingIdx[:0]
		}
		// R250-PERF-17 (#1120): apply the same shrink rule to idxScratch.
		// selectForIdx keeps `kept` pointing at the per-writer scratch
		// (assigned to w.idxScratch above when stride > 1), so an
		// InjectHistory burst inflates this slice's cap symmetrically with
		// pendingIdx; without this reset, 100+ active writers would each
		// pin a multi-KB scratch slice for the writer's lifetime even
		// after returning to steady-state batch sizes.
		if p.opts.IdxStride > 1 && cap(w.idxScratch) > p.opts.IdxStride*4 {
			w.idxScratch = make([]schema.IdxEntry, 0, p.opts.IdxStride*2)
		}
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
		// R249-PERF-21 (#995): return the bufio.Writer to the pool so the
		// 64 KiB buffer is reusable by the next perKeyWriter instead of
		// being released to GC. releaseLogBuf rebinds to io.Discard so the
		// nilled w.logBuf cannot accidentally route writes through the
		// pooled instance later.
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
