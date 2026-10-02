package persist

import (
	"log/slog"
	"time"
)

// failure.go — what the run goroutine does after a write or flush fails.
//
// A *bufio.Writer latches its first error: every later Write and Flush
// returns it without touching the fd. So a writer whose log buffer hit
// ENOSPC once stays dead after the disk frees up — every later event of the
// session is dropped while the flush warning repeats on each tick. Such a
// writer is marked poisoned and retired instead of retried: the next batch
// reopens the pair through writerFor, whose Recover cuts the torn log tail
// back to the idx edge, and writes resume once the disk has room.
//
// Failures are logged at most once per failureWarnEvery per key so a disk
// that stays full does not flood the service log.

// failureWarnEvery bounds how often one key's write-path failure is logged.
const failureWarnEvery = time.Minute

// failureState is one key's run of consecutive write-path failures. It
// outlives the retired writer so the throttle and the failure count carry
// across reopen cycles. Run-goroutine only.
type failureState struct {
	failures   int
	suppressed int
	lastWarn   time.Time
}

// noteFailure records a write-path failure for key, logging the first one
// and then at most once per failureWarnEvery with the suppressed count.
func (p *Persister) noteFailure(key, stage string, err error) {
	now := p.opts.Clock()
	st, ok := p.failing[key]
	if !ok {
		st = &failureState{}
		p.failing[key] = st
	}
	st.failures++
	if ok && now.Sub(st.lastWarn) < failureWarnEvery {
		st.suppressed++
		return
	}
	slog.Warn("event log persist: write path failed",
		"key", key, "stage", stage, "err", err,
		"failures", st.failures, "suppressed", st.suppressed)
	st.lastWarn = now
	st.suppressed = 0
}

// noteRecovered clears key's failure run after a successful flush.
func (p *Persister) noteRecovered(key string) {
	st, ok := p.failing[key]
	if !ok {
		return
	}
	delete(p.failing, key)
	slog.Info("event log persist: write path recovered",
		"key", key, "failures", st.failures)
}

// settleFlush is the run-goroutine half of a flush: flush itself may run on
// a parallelFsync worker, which must not touch p.writers or p.failing.
func (p *Persister) settleFlush(key string, w *perKeyWriter, stage string, err error) {
	if err == nil {
		if len(p.failing) > 0 {
			p.noteRecovered(key)
		}
		return
	}
	p.noteFailure(key, stage, err)
	if w.poisoned {
		p.retireWriter(key, w)
	}
}

// retireWriter closes a poisoned writer and forgets it so the next batch
// reopens the pair through Recover. Every record still in pendingIdx is
// counted as dropped: Recover truncates the log to the idx edge, so they are
// lost unless their idx entries reached the file. That makes the count an
// upper bound: a torn idx append may have landed a few, and after an idx
// fsync failure the appended entries usually all survive.
func (p *Persister) retireWriter(key string, w *perKeyWriter) {
	if lost := len(w.pendingIdx); lost > 0 {
		p.droppedCnt.Add(int64(lost))
		p.opts.Observer.OnDrop(lost)
	}
	// close() re-reports the latched error; the fds are released regardless.
	_ = w.close()
	if p.writers[key] == w {
		delete(p.writers, key)
	}
}
