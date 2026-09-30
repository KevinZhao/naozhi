package session

import (
	"log/slog"
	"time"
)

// RecordRetired stamps the retirement instant for sessionID and invalidates
// the history cache so the retired session shows on the next poll. The stamp
// is skipped when the store is unconfigured (and the store ignores an empty
// sessionID, from a CLI that never returned a UUID); the invalidation never is.
func (h *Handlers) RecordRetired(sessionID string) {
	if h.deps.RetiredStore != nil {
		h.deps.RetiredStore.MarkRetired(sessionID, time.Now())
	}
	h.InvalidateHistoryCache()
}

// FlushRetiredStore writes pending retired-at marks to disk at server
// shutdown. No-op without a store; errors are logged, not returned, so
// shutdown doesn't fail.
func (h *Handlers) FlushRetiredStore() {
	if h.deps.RetiredStore == nil {
		return
	}
	if err := h.deps.RetiredStore.Save(); err != nil {
		slog.Warn("flush retired store failed", "err", err)
	}
}

// RetiredStorePresent reports whether the RetiredStore is wired (server
// shutdown Prune gate).
func (h *Handlers) RetiredStorePresent() bool { return h.deps.RetiredStore != nil }

// PruneRetiredStore prunes entries older than cutoffMs; no-op when unwired.
func (h *Handlers) PruneRetiredStore(cutoffMs int64) {
	if h.deps.RetiredStore != nil {
		h.deps.RetiredStore.Prune(cutoffMs)
	}
}
