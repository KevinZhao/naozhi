package project

import "sync"

// uploadQuota bounds the cumulative bytes the upload endpoint accepts per
// project within a process lifetime (#2311): without it the ~10 req/min/IP
// limiter and 256 MiB per-file cap left any authenticated caller a
// ~2.5 GiB/min disk-fill primitive against every other tenant on a shared
// box. The counter is in-memory and per process — it resets on restart and
// does not count pre-existing files — so it is a fill-rate ceiling, not a
// filesystem quota. A zero or negative limit disables enforcement.
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
