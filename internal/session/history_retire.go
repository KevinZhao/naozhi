package session

import (
	"context"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/eventlog/ring"
)

// retireWaitMax caps how long a same-key session's sink binding waits for a
// removal's drop and clear: DropKey's 2s plus the attachment walk's 5s, with
// headroom. On timeout the sink binds anyway, as a failed drop would leave it.
const retireWaitMax = 8 * time.Second

// retireBarrier is held while one or more removals of a key drop its event
// log and attachment refs; done closes when the last of them ends.
type retireBarrier struct {
	n    int
	done chan struct{}
}

// eventLogHolder is the part of *cli.Process the persist sink binds to.
type eventLogHolder interface {
	EventLog() *ring.EventLog
}

var _ eventLogHolder = (*cli.Process)(nil)

// beginRetire raises key's barrier for the removal of s, marks s retired so
// its own late installPersistSink binds nothing, and reports whether it did;
// false when nothing is persisted. Called inside the table transaction that
// removes key, so no same-key session can commit and bind its sink first.
//
// LOCK: retireMu is a leaf below the table lock: it never takes the table
// lock and nothing under it blocks or does I/O.
func (h *HistoryIO) beginRetire(key string, s *ManagedSession) bool {
	if h.persister == nil && h.tracker == nil {
		return false
	}
	h.retireMu.Lock()
	defer h.retireMu.Unlock()
	if s != nil {
		s.historyRetired = true
	}
	if h.retiring == nil {
		h.retiring = map[string]*retireBarrier{}
	}
	b := h.retiring[key]
	if b == nil {
		b = &retireBarrier{done: make(chan struct{})}
		h.retiring[key] = b
	}
	b.n++
	return true
}

// endRetire lowers key's barrier, releasing waiters when the last removal of
// key ends. Pairs with a beginRetire that returned true.
func (h *HistoryIO) endRetire(key string) {
	h.retireMu.Lock()
	defer h.retireMu.Unlock()
	b := h.retiring[key]
	if b == nil {
		return
	}
	if b.n--; b.n <= 0 {
		close(b.done)
		delete(h.retiring, key)
	}
}

// awaitRetire blocks until no removal of key holds its barrier, limit
// elapses, ctx ends or Shutdown cancels h.ctx. Reports false only when limit
// elapses.
func (h *HistoryIO) awaitRetire(ctx context.Context, key string, limit time.Duration) bool {
	h.retireMu.Lock()
	b := h.retiring[key]
	h.retireMu.Unlock()
	if b == nil {
		return true
	}
	var cancelled <-chan struct{}
	if h.ctx != nil {
		cancelled = h.ctx.Done()
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-b.done:
	case <-ctx.Done():
	case <-cancelled:
	case <-timer.C:
		return false
	}
	return true
}

// bindUnlessRetired binds key's persist sink to log unless s is retired.
// The check and the bind share retireMu with beginRetire, so a removal
// either sees the sink bound and detaches it, or the bind sees s retired.
func (h *HistoryIO) bindUnlessRetired(s *ManagedSession, log *ring.EventLog, key string) {
	h.retireMu.Lock()
	defer h.retireMu.Unlock()
	if s != nil && s.historyRetired {
		return
	}
	h.bindPersistSink(log, key)
}

// retireKeyHistory drops a removed session's event log and attachment refs,
// first detaching its process's persist sink so the dying process cannot
// write the log back. It runs before the process closes, and ends the
// barrier the removal raised, so a same-key session binds its sink only
// after the drop and records only its own entries. An Append that loaded
// the old sink just before the detach can still land one entry after it.
func (h *HistoryIO) retireKeyHistory(key string, snap removeSnapshot) {
	if snap.retiring {
		defer h.endRetire(key)
	}
	if holder, ok := snap.proc.(eventLogHolder); ok {
		if log := holder.EventLog(); log != nil {
			log.SetPersistSinkPair(nil, nil)
		}
	}
	h.dropEventLogForKey(key)
	h.clearAttachmentTrackerRefs(key, snap.workspace)
}
