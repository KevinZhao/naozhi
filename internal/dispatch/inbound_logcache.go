package dispatch

import (
	"log/slog"
	"sync"
)

// inboundLogMaxEntries bounds the per-(platform,user,chat) logger cache so a
// long-lived dispatcher serving many distinct chats cannot grow it without
// limit. When exceeded the whole map is dropped and rebuilt — the cache is a
// pure allocation optimization, so a rare cold rebuild only re-pays the
// slog.With cost we were paying every message before #2233.
const inboundLogMaxEntries = 4096

// inboundLogCache memoizes the sanitized-attr inbound logger keyed by the
// (platform, user, chat) triple. Loggers are immutable handler chains, so a
// cached *slog.Logger is safe to share across the concurrent inbound
// goroutines. Zero value is ready to use. R202606c-PERF-010 (#2233).
type inboundLogCache struct {
	mu sync.RWMutex
	m  map[string]*slog.Logger
}

// get returns the cached logger for key, or nil if absent.
func (c *inboundLogCache) get(key string) *slog.Logger {
	c.mu.RLock()
	lg := c.m[key]
	c.mu.RUnlock()
	return lg
}

// put stores lg under key, dropping the whole map first if it has grown past
// the entry cap (cheap bound without a full LRU).
func (c *inboundLogCache) put(key string, lg *slog.Logger) {
	c.mu.Lock()
	if c.m == nil || len(c.m) >= inboundLogMaxEntries {
		c.m = make(map[string]*slog.Logger, inboundLogMaxEntries/8)
	}
	c.m[key] = lg
	c.mu.Unlock()
}
