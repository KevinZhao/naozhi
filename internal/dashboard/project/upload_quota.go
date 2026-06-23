package project

import "sync"

// uploadQuota bounds the cumulative number of bytes the upload endpoint will
// accept per project within a process lifetime. R202606g-SEC-3 (#2311):
// HandleFilesUpload's only DoS guards were a shared ~10 req/min/IP rate limiter
// and a 256 MiB per-file cap, giving any authenticated caller a ~2.5 GiB/min/IP
// disk-fill primitive with no per-tenant ceiling. On a shared / multi-operator
// box that is a disk-exhaustion DoS against every other tenant. This adds a
// per-project running total so one project cannot monopolise disk through the
// upload endpoint; the limit is deliberately generous (the legitimate use case
// is pushing build artefacts) but finite.
//
// Scope and intentional limits: the counter is in-memory and per process, so it
// resets on restart and does NOT count files already on disk from a previous
// run or written outside the endpoint. It is a fill-RATE / per-session ceiling,
// not a true filesystem quota — a complete solution would reconcile against
// on-disk usage. That heavier accounting is out of scope here; the goal is to
// remove the unbounded single-tenant fill primitive with a small, allocation-
// free hot path. A zero or negative limit disables enforcement (back-compat for
// the single-operator model that accepts the original trade-off).
type uploadQuota struct {
	mu       sync.Mutex
	limit    int64
	consumed map[string]int64
}

func newUploadQuota(limit int64) *uploadQuota {
	if limit <= 0 {
		return nil
	}
	return &uploadQuota{limit: limit, consumed: make(map[string]int64)}
}

// reserve atomically attempts to charge n bytes against project's running
// total. It returns ok=false (and charges nothing) when the reservation would
// push the project over its limit, so the caller can reject the upload BEFORE
// writing any bytes. n<=0 is always allowed and charges nothing. A nil quota
// (enforcement disabled) always allows.
func (q *uploadQuota) reserve(project string, n int64) bool {
	if q == nil {
		return true
	}
	if n <= 0 {
		return true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	cur := q.consumed[project]
	if cur+n > q.limit {
		return false
	}
	q.consumed[project] = cur + n
	return true
}

// release returns a previously reserved (but ultimately unwritten) byte count
// to project's budget — used when the write fails after a successful reserve so
// a transient IO error does not permanently burn quota. Clamps at zero so a
// double release can never make the counter negative. n<=0 and a nil quota are
// no-ops.
func (q *uploadQuota) release(project string, n int64) {
	if q == nil || n <= 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	cur := q.consumed[project]
	cur -= n
	if cur <= 0 {
		delete(q.consumed, project)
		return
	}
	q.consumed[project] = cur
}
