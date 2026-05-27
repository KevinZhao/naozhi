package server

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// countingCronSessions is a CronView stub that records how many times
// KnownSessionIDs() is invoked. Used to pin the caching contract: a 1Hz
// dashboard poll must NOT rebuild the cron-known set on every tab refresh
// when the historyCache (120s TTL) is hot.
type countingCronSessions struct {
	calls atomic.Int64
	ids   map[string]bool
}

func (c *countingCronSessions) KnownSessionIDs() map[string]bool {
	c.calls.Add(1)
	out := make(map[string]bool, len(c.ids))
	for k, v := range c.ids {
		out[k] = v
	}
	return out
}

// EnsureStub / SetJobPrompt are no-ops to satisfy the CronView interface.
func (c *countingCronSessions) EnsureStub(string) bool            { return false }
func (c *countingCronSessions) SetJobPrompt(string, string) error { return nil }

// TestHistorySessions_KnownSessionIDsCachedAcross1HzPolls pins R242-PERF-7
// (#671): the server-side historyCache (120s TTL via singleflight in
// historySessions) MUST collapse N rapid polls into a single
// loadHistorySessions invocation, which means cronSessions.KnownSessionIDs()
// is called at most once across that window. Pre-fix, multiple dashboard
// tabs hitting 1Hz forced jobs×Recent(200) map rebuilds on every tab
// refresh.
//
// We invoke historySessions() many times back-to-back, then assert the
// counting stub saw at most one call. The first call is the cold-cache
// load; subsequent calls within the TTL must be cache hits and never
// reach the filter construction below the singleflight gate.
func TestHistorySessions_KnownSessionIDsCachedAcross1HzPolls(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	srv.sessionH.WaitWarmHistory()

	claudeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(claudeDir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv.sessionH.claudeDir = claudeDir

	// Seed at least one JSONL so the discovery scan returns non-nil and
	// the caller can distinguish "cold load" from "no claudeDir wired".
	makeProjectDir(t, claudeDir, "00000000-1111-2222-3333-aaaaaaaaaaaa")

	stub := &countingCronSessions{ids: map[string]bool{}}
	srv.sessionH.cronSessions = stub

	// Force a cold cache so the first historySessions() triggers a real
	// FS scan + filter construction.
	srv.sessionH.historyCacheMu.Lock()
	srv.sessionH.historyCache = nil
	srv.sessionH.historyCacheTime = time.Time{}
	srv.sessionH.historyCacheMu.Unlock()

	// 32 polls back-to-back. At 1Hz this is half a minute of dashboard
	// activity, well below the 120s cacheTTL — every call after the
	// first must be a cache hit.
	const polls = 32
	for i := 0; i < polls; i++ {
		_ = srv.sessionH.historySessions()
	}

	// Tolerate exactly one call (the cold-cache load). Two or more would
	// mean the historyCache or the singleflight gate is leaking — the
	// regression #671 explicitly warned about.
	got := stub.calls.Load()
	if got > 1 {
		t.Fatalf("KnownSessionIDs called %d times across %d polls; want ≤1 (history cache must collapse N tabs into one rebuild) — #671 cache may have regressed", got, polls)
	}
}
