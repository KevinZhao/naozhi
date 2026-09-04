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

// Defaults tuned for "dozens of sessions, hundreds of attachments/day".
const (
	DefaultChannelBuffer  = 1024
	DefaultCoalesceWindow = 1 * time.Second
	DefaultIdleCloseAfter = 10 * time.Minute
)

// Observer receives tracker-level lifecycle callbacks. Matches the shape of
// internal/eventlog/persist.Observer so the session layer can forward both
// into internal/metrics without translating.
type Observer interface {
	// OnReferenceBump fires once per .meta rewrite (coalesced, not per event).
	// n is the number of references folded into this write.
	OnReferenceBump(n int)
	// OnReferenceClear fires once per .meta rewrite during session removal.
	OnReferenceClear(n int)
	// OnMetaWriteError fires when UpdateMetaFile failed; path is the .meta path.
	OnMetaWriteError(path string, err error)
	// OnDrop fires when the ingest channel is full and the event is discarded.
	OnDrop(n int)
}

type noopObserver struct{}

func (noopObserver) OnReferenceBump(int)            {}
func (noopObserver) OnReferenceClear(int)           {}
func (noopObserver) OnMetaWriteError(string, error) {}
func (noopObserver) OnDrop(int)                     {}

// WorkspaceResolver maps a session key-hash to its absolute workspace path
// (session.Router owns the table). Return "" for an unknown session — the
// tracker then drops the bump rather than walk an arbitrary filesystem.
type WorkspaceResolver func(keyhash string) string

// Options configures a Tracker. Zero fields fall back to the Default* constants.
type Options struct {
	// Workspaces maps session keyhash → absolute workspace root. Required.
	Workspaces WorkspaceResolver

	// ChannelBuffer bounds the ingest queue; full triggers the OnDrop policy.
	ChannelBuffer int

	// CoalesceWindow is the debounce interval: repeated bumps on the same
	// (keyhash, absPath) within it trigger a single .meta write.
	CoalesceWindow time.Duration

	// Clock is injected for deterministic tests; nil uses time.Now.
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

	// pending holds bumps inside the coalesce window. Only the run goroutine
	// touches it; Stats() must read pendingSize, never len(pending).
	pending map[coalesceKey]pendingBump

	// pendingSize mirrors len(pending), kept in sync by the run goroutine.
	pendingSize atomic.Int64

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
	// Clear payload (session removal).
	clearWorkspace string
	// done is set for synchronous callers (Flush / OnSessionRemoved); nil for bumps.
	done chan error
}

// NewTracker spins up the worker goroutine and returns a ready Tracker.
// Opts.Workspaces must be non-nil.
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

// OnPersistedEntry is the hot-path hook for an EventEntry with ImagePaths
// reaching disk (replayPhase=false only). Non-blocking: a full channel drops
// the batch — the entry is durable, the attachment may merely be GC'd early.
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

// OnSessionRemoved walks the workspace's attachment directory and clears
// keyhash from every .meta file that references it. Blocks until the worker
// finishes; ctx bounds the wait so a slow filesystem cannot wedge Router.Remove.
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
	// After Stop() the first select may still buffer the job into t.in after
	// run() has drained and exited, so done would never be written; the
	// closeCh case returns at once instead of burning the whole ctx budget.
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-t.closeCh:
		return ErrTrackerClosed
	}
}

// Flush synchronously writes every pending coalesced bump. Used by tests
// and by Router.Shutdown so tracker state is on disk before exit.
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
	// Same drain-then-buffer race as OnSessionRemoved.
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-t.closeCh:
		return ErrTrackerClosed
	}
}

// Stop signals the worker to drain and exit. Idempotent; subsequent
// hot-path calls are no-ops.
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

// Stats is the counter snapshot exposed via /health.attachment_tracker.
type Stats struct {
	Written      int64
	Cleared      int64
	Dropped      int64
	Errors       int64
	Pending      int
	ChannelCap   int
	ChannelDepth int
	// LastDrainMs is how long ago the worker last processed a job; -1 means
	// never drained (distinct from 0 = within the last millisecond).
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
		Pending:      int(t.pendingSize.Load()),
		ChannelCap:   cap(t.in),
		ChannelDepth: len(t.in),
		LastDrainMs:  lastMs,
	}
}

// WriterAlive reports whether the worker can still accept and drain work:
// not closed AND (channel empty-and-not-full OR recent drain). The flagged
// failure mode is "queue non-empty AND no drain in 5s"; an idle tracker is
// alive. Mirrors persist.Persister.WriterAlive.
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

// ErrTrackerClosed is returned once Stop has been called; match with errors.Is.
var ErrTrackerClosed = errors.New("tracker: closed")

// --- worker ----------------------------------------------------

func (t *Tracker) run() {
	defer t.wg.Done()
	// Debounce tick = CoalesceWindow/4, clamped to [50ms, 1s].
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
		// Apply pending bumps first so the clear sweep sees post-bump refs.
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
			t.pendingSize.Add(1)
			continue
		}
		// Keep the highest timeMS but DO NOT reset flushAt: a hot key would
		// otherwise starve forever while GC reads stale on-disk meta.
		if job.timeMS > prev.timeMS {
			prev.timeMS = job.timeMS
		}
		t.pending[key] = prev
	}
}

func (t *Tracker) handleClear(job trackerJob) error {
	// O(files) walk; only on session removal (rare).
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
		// Abort once closing so a slow workspace cannot pin run() past Stop's
		// deadline; RemoveReference is idempotent so a partial clear is safe.
		select {
		case <-t.closeCh:
			return ErrTrackerClosed
		default:
		}
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
			select {
			case <-t.closeCh:
				return ErrTrackerClosed
			default:
			}
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

// flushDue writes every pending entry whose coalesce window has elapsed.
func (t *Tracker) flushDue() {
	if len(t.pending) == 0 {
		return
	}
	now := t.opts.Clock()
	deleted := 0
	for k, v := range t.pending {
		if v.flushAt.After(now) {
			continue
		}
		t.applyBump(k, v)
		delete(t.pending, k)
		deleted++
	}
	if deleted > 0 {
		t.pendingSize.Add(-int64(deleted))
	}
}

// flushAll writes every pending entry regardless of deadline.
func (t *Tracker) flushAll() {
	if len(t.pending) == 0 {
		return
	}
	deleted := 0
	for k, v := range t.pending {
		t.applyBump(k, v)
		delete(t.pending, k)
		deleted++
	}
	if deleted > 0 {
		t.pendingSize.Add(-int64(deleted))
	}
}

// applyBump performs the single-attachment read-modify-write. Errors are
// counted and surfaced to the Observer, never logged at ERROR (a file
// deleted between persist and bump is legitimate churn).
func (t *Tracker) applyBump(key coalesceKey, bump pendingBump) {
	metaPath := attachment.MetaPathFor(key.absPath)
	changed, err := attachment.UpdateMetaFile(metaPath, func(m *attachment.Meta) bool {
		addedRef := m.AddReference(key.keyhash)
		// Advance LastReferencedAt even when the keyhash was already present
		// so the retention window extends.
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

// resolveAttachmentPath turns a workspace-relative (or absolute) ImagePath
// into an absolute path rooted at workspace. Returns "" for paths that
// escape the workspace attachment subtree so a compromised EventEntry
// cannot make the tracker rewrite arbitrary .meta files.
func resolveAttachmentPath(workspace, p string) string {
	if p == "" {
		return ""
	}
	// Normalise separators so mixed slashes cannot sidestep the prefix check.
	p = strings.ReplaceAll(p, `\`, "/")
	if filepath.IsAbs(p) {
		cleaned := filepath.Clean(p)
		wsAbsRoot := filepath.Join(workspace, attachment.Dir)
		if !strings.HasPrefix(cleaned, wsAbsRoot+string(filepath.Separator)) {
			return ""
		}
		return cleaned
	}
	cleaned := filepath.Clean(p)
	if strings.HasPrefix(cleaned, "..") {
		return ""
	}
	if !strings.HasPrefix(cleaned, attachment.Dir+string(filepath.Separator)) &&
		cleaned != attachment.Dir {
		return ""
	}
	return filepath.Join(workspace, cleaned)
}
