package session

import (
	"log/slog"
	"time"
)

// RecordRetired stamps the retirement instant for sessionID and invalidates
// the history cache so the new ordering shows on the next poll. No-op when
// the store is unconfigured or sessionID is empty (CLI never returned a UUID).
func (h *Handlers) RecordRetired(sessionID string) {
	if h.deps.RetiredStore == nil || sessionID == "" {
		return
	}
	h.deps.RetiredStore.MarkRetired(sessionID, time.Now())
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
