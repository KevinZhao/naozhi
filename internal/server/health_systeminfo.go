// /health 用的 CLI 二进制 stat 缓存。
package server

import (
	"os"
	"sync"
	"time"
)

// cliAvailable reports whether the CLI binary at path is stat-able.
//
// Cached per path with a coarse TTL: an os.Stat on every authenticated
// /health poll would give a token-thief a filesystem timing oracle on the
// host's binary layout, while the TTL still lets a re-deployed binary
// surface within one window.
func cliAvailable(path string) bool {
	return cliAvailableAt(path, time.Now())
}

// cliAvailableAt is the clock-injection seam for cliAvailable.
func cliAvailableAt(path string, now time.Time) bool {
	if v, ok := cliAvailCache.Load(path); ok {
		entry := v.(cliAvailEntry)
		if now.Sub(entry.generatedAt) < cliAvailCacheTTL {
			return entry.available
		}
	}
	_, err := os.Stat(path)
	available := err == nil
	cliAvailCache.Store(path, cliAvailEntry{generatedAt: now, available: available})
	return available
}

// cliAvailEntry is a single cache record; generatedAt carries the monotonic
// clock so host suspend cannot prematurely expire it.
type cliAvailEntry struct {
	generatedAt time.Time
	available   bool
}

// cliAvailCacheTTL caps how long a stat result is reused; tests may shorten it.
var cliAvailCacheTTL = 60 * time.Second

// cliAvailCache memoises cliAvailable(path) → cliAvailEntry, keyed by path.
var cliAvailCache sync.Map
