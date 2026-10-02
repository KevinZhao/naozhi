package session

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/eventlog/ring"
)

// HistoryIO is Router's history facet: where transcripts and event logs live,
// the loader that reads a resume chain, the event-log persister and the
// attachment tracker it feeds, and the cancel/wait bookkeeping that lets
// Shutdown drain history goroutines. None of it touches the session table, so
// it holds no Router pointer; what it needs from the table (a backend's
// wrapper, a keyhash's workspace) Router resolves and passes in. Reach it
// through Router.History. The zero value (a hand-built test Router) has no
// ctx: tasks run on a never-cancelled background and nothing persists.
type HistoryIO struct {
	// claudeDir is the ~/.claude dir history is loaded from; "" skips every
	// JSONL load.
	claudeDir string
	// backendDirs maps a backend ID to its transcript directory, plumbed into
	// history.Wiring by attachHistorySource and read by resume validation. A
	// private copy of RouterConfig.BackendDirs.
	backendDirs map[string]string
	// loader loads a session's persisted JSONL history tail across a
	// prev_session_ids chain; tests inject a fixture (#458). Never nil after
	// NewRouter, read-only.
	loader HistoryLoader

	// wg tracks history goroutines (startup loads, resume loads, the orphan
	// sweep) so Shutdown waits for them.
	wg sync.WaitGroup
	// wgMu serialises the "check ctx.Err() then wg.Add(1)" pair against
	// cancelTasks (#2186): a cancel landing between the nil-Err check and the
	// Add could re-add to a WaitGroup already drained to 0 with a Wait in
	// flight ("WaitGroup is reused before previous Wait has returned").
	// cancelTasks takes this lock around cancel() (NOT around Wait), so a
	// producer that passed the check completes its Add before the cancel is
	// observable, and any later producer sees Err()!=nil and bails.
	wgMu sync.Mutex
	// ctx is cancelled on Shutdown so in-flight LoadHistory*Ctx calls abort
	// promptly instead of blocking the drain on slow filesystems. Paired with
	// cancel (set by NewRouter, called from cancelTasks).
	ctx    context.Context
	cancel context.CancelFunc

	// eventLogDir is where per-session event log files live. Empty disables
	// event log persistence (tests / opt-out); non-empty wires persister for
	// writes and naozhilog.Source for reads.
	eventLogDir string
	persister   *persist.Persister
	// tracker is the refcount tracker that bridges event-log persist events
	// to .meta sidecar updates. nil when eventLogDir is unset (no event
	// source). See docs/rfc/attachment-refcount.md.
	tracker *attachmentTracker
}

// History returns the router's history facet; nil for a nil Router.
func (r *Router) History() *HistoryIO {
	if r == nil {
		return nil
	}
	return &r.hist
}

// runHistoryTask launches fn in a goroutine tracked by h.wg, parented on h.ctx.
// Returns false (no goroutine) when ctx is already cancelled, guarding the
// late Add(1) race against wg.Wait() (#748); inline sites that also need a
// semaphore + per-task timeout stay in place.
//
// The wg.Add(1) must be visible to Shutdown before the goroutine begins
// observable work.
func (h *HistoryIO) runHistoryTask(fn func(ctx context.Context)) bool {
	if h.ctx == nil {
		// Test routers built by struct literal (skip NewRouter) get a
		// never-cancelled background; production Router always wires
		// the ctx in NewRouter before any caller can reach here.
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			fn(context.Background())
		}()
		return true
	}
	// Decide spawn-or-refuse BEFORE Add(1): an Add-then-compensating-Done shape
	// lets Shutdown's Wait() observe the transient +1 and return, after which a
	// later Add re-adds to a drained WaitGroup (#1655). The Err() check and the
	// Add(1) must be one critical section vs cancelTasks (#2186); see wgMu.
	h.wgMu.Lock()
	if h.ctx.Err() != nil {
		h.wgMu.Unlock()
		return false
	}
	h.wg.Add(1)
	h.wgMu.Unlock()
	go func() {
		defer h.wg.Done()
		fn(h.ctx)
	}()
	return true
}

// bindPersistSink wires the event-log persister, and through it the
// attachment tracker, into a process's EventLog under key. No-op when the
// persister is disabled or log is nil. installPersistSink is the caller; it is
// split out so a test can hand it an EventLog without a live CLI process.
func (h *HistoryIO) bindPersistSink(log *ring.EventLog, key string) {
	if h.persister == nil || log == nil {
		return
	}
	persisterSink := h.persister.SinkFor(key)
	keyhash := persist.KeyHash(key)
	sink := newEventLogSink(persisterSink, h.tracker, keyhash)
	// Single-entry sink lets EventLog.Append skip a 1-slot slice literal;
	// both sinks feed the same persisterSink (#410).
	sinkOne := newEventLogSinkOne(persisterSink, h.tracker, keyhash)
	log.SetPersistSinkPair(sink, sinkOne)
}

// cancelTasks cancels ctx under wgMu, so it is atomic vs the "check Err()
// then Add(1)" pair in runHistoryTask / loadResumeHistoryOnSpawn and a later
// waitTasks never races a late Add (#2186). No-op without a ctx.
func (h *HistoryIO) cancelTasks() {
	if h.cancel == nil {
		return
	}
	h.wgMu.Lock()
	h.cancel()
	h.wgMu.Unlock()
}

// waitTasks waits for the tracked history goroutines, but not past d: FS I/O
// can hang (e.g. NFS). Reports whether they all returned. On timeout the
// goroutine around wg.Wait() stays behind, which is bounded by Shutdown's
// single-shot contract. Do NOT replace wg.Wait() with a ctx-aware pattern:
// WaitGroup has none; the select IS the bounded wait.
func (h *HistoryIO) waitTasks(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		// Goroutine intentionally left running on timeout; cleaned up on process exit.
		h.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(d)
	select {
	case <-done:
		timer.Stop()
		return true
	case <-timer.C:
		return false
	}
}

// stopPersister flushes and stops the event-log persister so batches still in
// its in-channel reach disk. The ctx parent is context.Background, NOT h.ctx:
// cancelTasks has already cancelled that, so a child would see ctx.Err()
// immediately and the persister would skip flushing.
func (h *HistoryIO) stopPersister() {
	if h.persister == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.persister.Stop(ctx); err != nil {
		slog.Warn("event log persister stop timed out",
			"err", err, "stats", h.persister.Stats())
	}
}
