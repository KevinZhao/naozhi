package session

import (
	"context"
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/attachment/tracker"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/metrics"
)

// attachmentTracker is a type alias to the exported tracker.Tracker
// so router.go can hold a field typed by a short local name without
// adding another import.
type attachmentTracker = tracker.Tracker

// attachmentMetricsObserver forwards tracker callbacks to the
// process-wide expvar counters in internal/metrics. Lives in the
// session package so the tracker package stays independent of any
// metrics library.
type attachmentMetricsObserver struct{}

func (attachmentMetricsObserver) OnReferenceBump(n int) {
	if n <= 0 {
		return
	}
	metrics.AttachmentRefBumpTotal.Add(int64(n))
}

func (attachmentMetricsObserver) OnReferenceClear(n int) {
	if n <= 0 {
		return
	}
	metrics.AttachmentRefClearTotal.Add(int64(n))
}

func (attachmentMetricsObserver) OnMetaWriteError(path string, err error) {
	metrics.AttachmentRefMetaErrorTotal.Add(1)
	slog.Warn("attachment tracker: meta write failed",
		"path", path, "err", err)
}

func (attachmentMetricsObserver) OnDrop(n int) {
	if n <= 0 {
		return
	}
	metrics.AttachmentRefDropTotal.Add(int64(n))
}

// workspaceResolverForTracker returns a WorkspaceResolver closure keyed by
// session key-hash. Matches the tracker's contract: empty string on unknown
// keyhash → tracker drops the bump silently.
//
// Runs on every persisted image-bearing event on the tracker worker goroutine,
// so it consults r.ss.keyhash for an O(1) lookup and only falls back to the
// O(N)-hash scan when the index misses (the index is a pure fast-path, never
// the source of truth) (#1646).
func (r *Router) workspaceResolverForTracker() tracker.WorkspaceResolver {
	return func(keyhash string) string {
		if keyhash == "" {
			return ""
		}
		r.mu.RLock()
		defer r.mu.RUnlock()
		// Fast path re-verified against r.ss.sessions so a stale index entry
		// degrades to the scan rather than returning a dead session's workspace.
		if key, ok := r.ss.keyhash[keyhash]; ok {
			if s := r.ss.sessions[key]; s != nil && persist.KeyHash(key) == keyhash {
				return s.Workspace()
			}
		}
		// Fallback scan covers test routers with a nil index and the stale-index
		// case. Read-only — repairing the index would need the write lock.
		for k, s := range r.ss.sessions {
			if persist.KeyHash(k) == keyhash {
				return s.Workspace()
			}
		}
		return ""
	}
}

// startAttachmentTracker spins up the tracker bound to r's eventLogDir +
// session table. Called from NewRouter AFTER the persister + session map are
// constructed so the resolver closure is ready to serve lookups. On init
// failure we log + continue without tracking; attachments then fall back to
// pure upload-TTL GC.
func (r *Router) startAttachmentTracker() {
	if r.eventLogDir == "" {
		// Without the event-log persistence tier no OnPersistedEntry signals
		// arrive, so the tracker would never bump.
		return
	}
	t, err := tracker.NewTracker(tracker.Options{
		Workspaces: r.workspaceResolverForTracker(),
		Observer:   attachmentMetricsObserver{},
	})
	if err != nil {
		slog.Error("attachment tracker init failed; refcount disabled",
			"err", err)
		return
	}
	r.attachmentTracker = t
}

// stopAttachmentTracker flushes pending bumps and releases the worker
// goroutine. Called from Router.shutdown AFTER the persister has stopped so no
// more OnPersistedEntry callbacks arrive while draining.
//
// Intentionally parents on context.Background, NOT r.historyCtx: shutdown's
// first action cancels historyCtx, so deriving from it would give the drain
// loop zero time to flush. The 5s budget is the load-bearing bound.
func (r *Router) stopAttachmentTracker() {
	if r.attachmentTracker == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.attachmentTracker.Stop(ctx); err != nil {
		slog.Warn("attachment tracker stop timed out",
			"err", err, "stats", r.attachmentTracker.Stats())
	}
}
