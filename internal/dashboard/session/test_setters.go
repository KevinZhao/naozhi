// Phase 3e (server-split-phase4-design.md §6.5 Plan B): test-only setters
// expose unexported fields so internal/server's integration tests
// (4 dashboard_session_*_test.go files that depend on newTestServer
// fixtures) can wire them after server.New runs but before exercising
// HandleX. NOT for production use — production paths construct the
// Handlers via New(Deps{...}).
//
// File name suffixed _test.go is intentional in some cases but here we
// keep it as a non-test file so the setters are compiled into the
// server's test binary too. They have no production code path.
package session

import (
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/discovery"
)

// SetClaudeDirForTest swaps the runtime claudeDir.
func (h *Handlers) SetClaudeDirForTest(dir string) { h.claudeDir = dir }

// SetSysWorkDirForTest swaps the sysWorkDir.
func (h *Handlers) SetSysWorkDirForTest(dir string) { h.sysWorkDir = dir }

// SetCronSessionsForTest swaps the cronSessions view.
func (h *Handlers) SetCronSessionsForTest(c CronView) { h.cronSessions = c }

// ResetHistoryCacheForTest clears the in-memory history cache so the next
// call to loadHistorySessions / historySessions goes back to disk.
func (h *Handlers) ResetHistoryCacheForTest() {
	h.historyCacheMu.Lock()
	h.historyCache = nil
	h.historyCacheTime = time.Time{}
	h.historyCacheTimeUnixNano.Store(0)
	h.historyCacheMu.Unlock()
}

// LoadHistorySessionsForTest is the test-visible alias for the unexported
// loadHistorySessions method.
func (h *Handlers) LoadHistorySessionsForTest() []discovery.RecentSession {
	return h.loadHistorySessions()
}

// HistorySessionsForTest is the test-visible alias for the unexported
// historySessions method.
func (h *Handlers) HistorySessionsForTest() []discovery.RecentSession {
	return h.historySessions()
}

// HistoryCacheLockForTest is the test-visible accessor for the cache mutex.
// Tests that need to manipulate cache state under the lock can hold this
// while assigning through SetCachedHistoryForTest.
func (h *Handlers) HistoryCacheLockForTest() *sync.Mutex { return &h.historyCacheMu }

// SetCachedHistoryForTest atomically replaces the cached slice + timestamp.
// Caller MUST hold HistoryCacheLockForTest().
func (h *Handlers) SetCachedHistoryForTest(slice []discovery.RecentSession, t time.Time) {
	h.historyCache = slice
	h.historyCacheTime = t
	h.historyCacheTimeUnixNano.Store(t.UnixNano())
}

// RetiredStoreForTest exposes the retiredStore field for tests that need
// to assert RecordRetired/Prune behaviour.
func (h *Handlers) RetiredStoreForTest() *discovery.RetiredStore { return h.retiredStore }

// SetRetiredStoreForTest swaps the retiredStore.
func (h *Handlers) SetRetiredStoreForTest(s *discovery.RetiredStore) {
	h.retiredStore = s
}

// SetHistoryCacheTimeForTest assigns historyCacheTime under the cache mutex
// (and mirrors atomic). Tests use this to fake "fresh cache".
func (h *Handlers) SetHistoryCacheTimeForTest(t time.Time) {
	h.historyCacheMu.Lock()
	defer h.historyCacheMu.Unlock()
	h.historyCacheTime = t
	h.historyCacheTimeUnixNano.Store(t.UnixNano())
}

// IsHistoryCacheTimeZeroForTest reports whether historyCacheTime is the
// zero value. Used to assert RecordRetired's invalidation logic.
func (h *Handlers) IsHistoryCacheTimeZeroForTest() bool {
	h.historyCacheMu.Lock()
	defer h.historyCacheMu.Unlock()
	return h.historyCacheTime.IsZero()
}

// HistoryCacheTimeForTest returns the current cached time. Used by tests
// that assert TTL behaviour.
func (h *Handlers) HistoryCacheTimeForTest() time.Time {
	h.historyCacheMu.Lock()
	defer h.historyCacheMu.Unlock()
	return h.historyCacheTime
}
