package tracker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/attachment"
)

// Defaults tuned for naozhi's "dozens of sessions, hundreds of
// attachments/day" profile. Tune via Options.
const (
	DefaultChannelBuffer  = 1024
	DefaultCoalesceWindow = 1 * time.Second
	DefaultIdleCloseAfter = 10 * time.Minute
)

// Observer receives tracker-level lifecycle callbacks. Matches the
// shape of internal/eventlog/persist.Observer so the session layer
// can forward both into internal/metrics without translating.
type Observer interface {
	// OnReferenceBump fires once per .meta file rewrite (NOT once
	// per incoming event — coalesced). n is the number of references
	// folded into this write; 1 for singleton, >1 when coalesce
	// batched multiple bumps to the same attachment.
	OnReferenceBump(n int)
	// OnReferenceClear fires once per .meta file rewrite during
	// session removal. n is the number of files whose keyhash was
	// cleared.
	OnReferenceClear(n int)
	// OnMetaWriteError fires when UpdateMetaFile returned an error
	// (missing sidecar, disk full, …). path is the absolute .meta
	// path; implementations typically log + increment a counter.
	OnMetaWriteError(path string, err error)
	// OnDrop fires when the worker's channel is full at enqueue
	// time and the event is discarded. Mirrors the persist-layer
	// drop counter so operators have a single metric family to
	// monitor.
	OnDrop(n int)
}

type noopObserver struct{}

func (noopObserver) OnReferenceBump(int)            {}
func (noopObserver) OnReferenceClear(int)           {}
func (noopObserver) OnMetaWriteError(string, error) {}
func (noopObserver) OnDrop(int)                     {}

// WorkspaceResolver maps a session key-hash to the absolute workspace
// path where that session's event log stored its ImagePaths. The
// tracker does NOT own the key → workspace table (session.Router
// does); passing the closure keeps this package decoupled from the
// session package.
//
// Return "" when the session is unknown; the tracker then drops
// the bump with a Debug log rather than walk an arbitrary filesystem.
type WorkspaceResolver func(keyhash string) string

// Options configures a Tracker. Zero fields fall back to the
// Default* constants.
type Options struct {
	// Workspaces maps session keyhash → absolute workspace root.
	// Required: a nil resolver disables every bump (the tracker
	// becomes a no-op).
	Workspaces WorkspaceResolver

	// ChannelBuffer bounds the size of the ingest queue. Full
	// channel triggers the drop policy documented in OnDrop.
	ChannelBuffer int

	// CoalesceWindow is the debounce interval: repeated bumps on
	// the same (keyhash, absPath) pair within this window only
	// trigger a single .meta write at the window's end.
	CoalesceWindow time.Duration

	// Clock is injected for deterministic tests. Production leaves
	// it nil and uses time.Now.
	Clock func() time.Time

	// Observer receives metric callbacks. nil → noop.
	Observer Observer
}

// Tracker is the exported type. See package godoc for lifecycle.
type Tracker struct {
	opts    Options
	in      chan trackerJob
	closeCh chan struct{}
	closed  atomic.Bool
	wg      sync.WaitGroup

	// pending holds bumps that are inside the coalesce window. Keys
	// are (keyhash, absPath) tuples; values capture the most recent
	// observed timeMS. Single-writer — only the run goroutine
	// touches this.
	pending map[coalesceKey]pendingBump

	// writtenCnt / clearCnt / droppedCnt mirror Observer callbacks
	// for test introspection and /health stats snapshot.
	writtenCnt atomic.Int64
	clearCnt   atomic.Int64
	droppedCnt atomic.Int64
	errorCnt   atomic.Int64

	lastDrainNS atomic.Int64
}

type coalesceKey struct {
	keyhash string
	absPath string
}

type pendingBump struct {
	timeMS  int64
	flushAt time.Time
}

// trackerJob is the union type the channel carries.
type trackerJobKind int

const (
	jobKindBump trackerJobKind = iota
	jobKindClear
	jobKindFlush
)

type trackerJob struct {
	kind    trackerJobKind
	keyhash string
	// Bump payload.
	absPaths []string
	timeMS   int64
	// Clear payload ← session removal.
	clearWorkspace string
	// Common done channel for synchronous callers (used by Flush /
	// OnSessionRemoved); nil for async bumps.
	done chan error
}

// NewTracker spins up the worker goroutine and returns a ready
// Tracker. Opts.Workspaces must be non-nil; otherwise the tracker
// would have no way to turn a keyhash into an attachment path.
func NewTracker(opts Options) (*Tracker, error) {
	if opts.Workspaces == nil {
		return nil, errors.New("tracker: Options.Workspaces is required")
	}
	if opts.ChannelBuffer <= 0 {
		opts.ChannelBuffer = DefaultChannelBuffer
	}
	if opts.CoalesceWindow <= 0 {
		opts.CoalesceWindow = DefaultCoalesceWindow
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.Observer == nil {
		opts.Observer = noopObserver{}
	}
	t := &Tracker{
		opts:    opts,
		in:      make(chan trackerJob, opts.ChannelBuffer),
		closeCh: make(chan struct{}),
		pending: make(map[coalesceKey]pendingBump),
	}
	t.wg.Add(1)
	go t.run()
	return t, nil
}

// OnPersistedEntry is the hot-path hook the session-layer sink
// bridge calls whenever an EventEntry with non-empty ImagePaths
// reaches disk (replayPhase=false only). It enqueues a bump for
// every path; the worker coalesces.
//
// Non-blocking: full channel drops the entire batch with a counter
// increment. The event-log entry itself is already durable — a
// dropped tracker bump only means the attachment may be collected
// a little early under heavy load.
func (t *Tracker) OnPersistedEntry(keyhash string, absPaths []string, timeMS int64) {
	if t == nil || t.closed.Load() {
		return
	}
	if keyhash == "" || len(absPaths) == 0 {
		return
	}
	job := trackerJob{
		kind:     jobKindBump,
		keyhash:  keyhash,
		absPaths: absPaths,
		timeMS:   timeMS,
	}
	select {
	case t.in <- job:
	default:
		t.droppedCnt.Add(1)
		t.opts.Observer.OnDrop(1)
		slog.Warn("attachment tracker: channel full; dropping bump",
			"keyhash", keyhash, "paths", len(absPaths),
			"channel_cap", cap(t.in))
	}
}

// OnSessionRemoved walks the workspace's attachment directory and
// clears keyhash from every .meta file that references it. Blocks
// until the worker finishes the walk (sessions are deleted
// infrequently; simplicity beats async semantics here).
//
// ctx timeout enforces a hard upper bound so a slow filesystem can't
// wedge Router.Remove indefinitely.
func (t *Tracker) OnSessionRemoved(ctx context.Context, keyhash, workspace string) error {
	if t == nil || t.closed.Load() {
		return nil
	}
	if keyhash == "" || workspace == "" {
		return nil
	}
	done := make(chan error, 1)
	job := trackerJob{
		kind:           jobKindClear,
		keyhash:        keyhash,
		clearWorkspace: workspace,
		done:           done,
	}
	select {
	case t.in <- job:
	case <-ctx.Done():
		return ctx.Err()
	case <-t.closeCh:
		return ErrTrackerClosed
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Flush synchronously writes every pending coalesced bump. Used by
// tests and by Router.Shutdown to guarantee tracker state is on
// disk before process exit.
func (t *Tracker) Flush(ctx context.Context) error {
	if t == nil || t.closed.Load() {
		return nil
	}
	done := make(chan error, 1)
	job := trackerJob{kind: jobKindFlush, done: done}
	select {
	case t.in <- job:
	case <-ctx.Done():
		return ctx.Err()
	case <-t.closeCh:
		return ErrTrackerClosed
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop signals the worker to drain and exit. Idempotent. Safe to
// call after Stop — subsequent hot-path calls are no-ops.
func (t *Tracker) Stop(ctx context.Context) error {
	if t == nil {
		return nil
	}
	if !t.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(t.closeCh)
	doneCh := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stats returns a snapshot of the counters the tracker exposes via
// /health.attachment_tracker.
type Stats struct {
	Written      int64
	Cleared      int64
	Dropped      int64
	Errors       int64
	Pending      int
	ChannelCap   int
	ChannelDepth int
	// LastDrainMs is how long ago the worker last processed a job,
	// in milliseconds. -1 means "never drained" (the worker has not
	// handled a single job yet); distinct from 0 which means
	// "drained within the last millisecond".
	LastDrainMs int64
}

func (t *Tracker) Stats() Stats {
	if t == nil {
		return Stats{}
	}
	var lastMs int64
	ns := t.lastDrainNS.Load()
	if ns == 0 {
		lastMs = -1
	} else {
		lastMs = time.Duration(t.opts.Clock().UnixNano() - ns).Milliseconds()
	}
	return Stats{
		Written:      t.writtenCnt.Load(),
		Cleared:      t.clearCnt.Load(),
		Dropped:      t.droppedCnt.Load(),
		Errors:       t.errorCnt.Load(),
		Pending:      len(t.pending), // race-tolerable: informational
		ChannelCap:   cap(t.in),
		ChannelDepth: len(t.in),
		LastDrainMs:  lastMs,
	}
}

// WriterAlive reports whether the worker goroutine can still accept
// and drain work. A healthy tracker is NOT required to have seen a
// recent drain — an idle session with no image events is still alive,
// just quiet. The liveness signal is:
//
//	not closed AND (channel is empty-and-not-full OR recent drain)
//
// The empty-channel shortcut covers cold-start (never drained) and
// long idle periods (no image events in hours). The recent-drain
// branch catches the "channel has work and worker is making progress"
// case. The failure mode we want to flag is "channel has work but
// worker stalled" — i.e. queue non-empty AND no drain in 5s.
//
// Production /health consumes this directly. See the mirroring
// implementation in persist.Persister.WriterAlive.
func (t *Tracker) WriterAlive() bool {
	if t == nil || t.closed.Load() {
		return false
	}
	s := t.Stats()
	if s.ChannelCap == 0 {
		return false
	}
	notFull := s.ChannelDepth*5 < s.ChannelCap*4
	if s.ChannelDepth == 0 {
		return notFull
	}
	drainedRecently := s.LastDrainMs >= 0 && s.LastDrainMs < 5000
	return drainedRecently && notFull
}

// Errors callers match with errors.Is.
var ErrTrackerClosed = errors.New("tracker: closed")

// --- worker ----------------------------------------------------

func (t *Tracker) run() {
	defer t.wg.Done()
	// Debounce tick granularity = CoalesceWindow/4, bounded on both
	// sides to avoid pathological intervals on extreme configs.
	tickEvery := t.opts.CoalesceWindow / 4
	if tickEvery < 50*time.Millisecond {
		tickEvery = 50 * time.Millisecond
	}
	if tickEvery > time.Second {
		tickEvery = time.Second
	}
	tick := time.NewTicker(tickEvery)
	defer tick.Stop()

	for {
		select {
		case job := <-t.in:
			t.handleJob(job)
			t.lastDrainNS.Store(t.opts.Clock().UnixNano())
		case <-tick.C:
			t.flushDue()
		case <-t.closeCh:
			// Drain queued work and flush pending before exit.
			for {
				select {
				case job := <-t.in:
					t.handleJob(job)
				default:
					t.flushAll()
					return
				}
			}
		}
	}
}

func (t *Tracker) handleJob(job trackerJob) {
	switch job.kind {
	case jobKindBump:
		t.handleBump(job)
	case jobKindClear:
		// Clear callers expect the observe + pending bumps applied
		// first so OnSessionRemoved's sweep sees the post-bump refs
		// rather than a stale subset.
		t.flushAll()
		err := t.handleClear(job)
		if job.done != nil {
			job.done <- err
		}
	case jobKindFlush:
		t.flushAll()
		if job.done != nil {
			job.done <- nil
		}
	}
}

func (t *Tracker) handleBump(job trackerJob) {
	workspace := t.opts.Workspaces(job.keyhash)
	if workspace == "" {
		slog.Debug("attachment tracker: unknown workspace for keyhash; dropping bump",
			"keyhash", job.keyhash)
		return
	}
	flushAt := t.opts.Clock().Add(t.opts.CoalesceWindow)
	for _, relOrAbs := range job.absPaths {
		abs := resolveAttachmentPath(workspace, relOrAbs)
		if abs == "" {
			continue
		}
		key := coalesceKey{keyhash: job.keyhash, absPath: abs}
		prev, ok := t.pending[key]
		if !ok {
			t.pending[key] = pendingBump{timeMS: job.timeMS, flushAt: flushAt}
			continue
		}
		// Keep the highest timeMS observed and push the flushAt
		// deadline out — coalesce semantics.
		if job.timeMS > prev.timeMS {
			prev.timeMS = job.timeMS
		}
		prev.flushAt = flushAt
		t.pending[key] = prev
	}
}

func (t *Tracker) handleClear(job trackerJob) error {
	// A full walk of the workspace's attachment directory. This is
	// O(files) but happens only on session removal (rare). We read
	// every .meta, remove the keyhash, rewrite on change.
	root := filepath.Join(job.clearWorkspace, attachment.Dir)
	dayEntries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read attachments root %s: %w", root, err)
	}
	cleared := 0
	for _, de := range dayEntries {
		if !de.IsDir() {
			continue
		}
		dayPath := filepath.Join(root, de.Name())
		files, err := os.ReadDir(dayPath)
		if err != nil {
			slog.Warn("attachment tracker: read day dir failed",
				"dir", dayPath, "err", err)
			continue
		}
		for _, fe := range files {
			name := fe.Name()
			if !strings.HasSuffix(name, ".meta") {
				continue
			}
			metaPath := filepath.Join(dayPath, name)
			changed, err := attachment.UpdateMetaFile(metaPath,
				func(m *attachment.Meta) bool {
					return m.RemoveReference(job.keyhash)
				},
			)
			if err != nil {
				t.errorCnt.Add(1)
				t.opts.Observer.OnMetaWriteError(metaPath, err)
				continue
			}
			if changed {
				cleared++
			}
		}
	}
	if cleared > 0 {
		t.clearCnt.Add(int64(cleared))
		t.opts.Observer.OnReferenceClear(cleared)
	}
	return nil
}

// flushDue writes every pending entry whose coalesce window has
// elapsed. Called from the debounce ticker.
func (t *Tracker) flushDue() {
	if len(t.pending) == 0 {
		return
	}
	now := t.opts.Clock()
	for k, v := range t.pending {
		if v.flushAt.After(now) {
			continue
		}
		t.applyBump(k, v)
		delete(t.pending, k)
	}
}

// flushAll writes every pending entry regardless of deadline. Used
// by Flush and by graceful Stop.
func (t *Tracker) flushAll() {
	if len(t.pending) == 0 {
		return
	}
	for k, v := range t.pending {
		t.applyBump(k, v)
		delete(t.pending, k)
	}
}

// applyBump performs the single-attachment read-modify-write. Any
// error is counted and surfaced to the Observer but never logged
// at ERROR level to avoid log spam for legitimate churn (e.g. a
// file deleted between persist and bump).
func (t *Tracker) applyBump(key coalesceKey, bump pendingBump) {
	metaPath := metaPathFor(key.absPath)
	changed, err := attachment.UpdateMetaFile(metaPath, func(m *attachment.Meta) bool {
		addedRef := m.AddReference(key.keyhash)
		// Always advance LastReferencedAt to max(current, bump) —
		// even if the keyhash was already present, we want to
		// extend the retention window.
		if bump.timeMS > m.LastReferencedAt {
			m.LastReferencedAt = bump.timeMS
			return true
		}
		return addedRef
	})
	if err != nil {
		t.errorCnt.Add(1)
		t.opts.Observer.OnMetaWriteError(metaPath, err)
		return
	}
	if changed {
		t.writtenCnt.Add(1)
		t.opts.Observer.OnReferenceBump(1)
	}
}

// resolveAttachmentPath turns a workspace-relative (or already
// absolute) ImagePath into an absolute path rooted at workspace.
// Returns "" for paths that escape the workspace attachment subtree
// — defensive guard so a compromised EventEntry cannot coax the
// tracker into rewriting arbitrary .meta files.
func resolveAttachmentPath(workspace, p string) string {
	if p == "" {
		return ""
	}
	// Normalise separator so mixed forward-slash / back-slash (from
	// a Windows client) cannot sidestep the prefix check.
	p = strings.ReplaceAll(p, `\`, "/")
	if filepath.IsAbs(p) {
		// Absolute path: require it to live under workspace/Dir.
		cleaned := filepath.Clean(p)
		wsAbsRoot := filepath.Join(workspace, attachment.Dir)
		if !strings.HasPrefix(cleaned, wsAbsRoot+string(filepath.Separator)) {
			return ""
		}
		return cleaned
	}
	// Workspace-relative path. Clean + guard against "../" escapes.
	cleaned := filepath.Clean(p)
	if strings.HasPrefix(cleaned, "..") {
		return ""
	}
	// Only paths that begin with the known attachment subtree are
	// accepted; anything else is not an attachment.
	if !strings.HasPrefix(cleaned, attachment.Dir+string(filepath.Separator)) &&
		cleaned != attachment.Dir {
		return ""
	}
	return filepath.Join(workspace, cleaned)
}

// metaPathFor mirrors attachment.metaPathFor but is re-exported so
// tracker tests can round-trip without crossing the package
// boundary. Strip the payload extension, append .meta.
func metaPathFor(payload string) string {
	base := filepath.Base(payload)
	if idx := strings.LastIndex(base, "."); idx > 0 {
		return filepath.Join(filepath.Dir(payload), base[:idx]+".meta")
	}
	return payload + ".meta"
}
