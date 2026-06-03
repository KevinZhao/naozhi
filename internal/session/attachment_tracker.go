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

// workspaceResolverForTracker returns a WorkspaceResolver closure
// keyed by session key-hash. Matches the tracker's contract: empty
// string on unknown keyhash → tracker drops the bump silently.
//
// #1646: this runs on every persisted image-bearing event (potentially
// several per Send) on the tracker worker goroutine. It used to hold
// r.mu.RLock and linearly scan r.sessions, recomputing a SHA-256 KeyHash
// per session, which is O(N)-hashes per bump on image-rich / cron-heavy
// stores. It now consults r.keyhashToKey for an O(1) lookup and only falls
// back to the scan when the index misses or points at a since-removed key
// (self-healing — the index is a pure fast-path, never the source of truth).
func (r *Router) workspaceResolverForTracker() tracker.WorkspaceResolver {
	return func(keyhash string) string {
		if keyhash == "" {
			return ""
		}
		r.mu.RLock()
		defer r.mu.RUnlock()
		// Fast path: O(1) index lookup, re-verified against r.sessions so a
		// stale entry (delete site that bypassed indexDel) degrades to the
		// scan below rather than returning a workspace for a dead session.
		if key, ok := r.keyhashToKey[keyhash]; ok {
			if s := r.sessions[key]; s != nil && persist.KeyHash(key) == keyhash {
				return s.Workspace()
			}
		}
		// Fallback: linear scan (legacy behaviour). Covers test routers with a
		// nil index and the rare stale-index case. Read-only — repairing the
		// index would need the write lock we deliberately don't take here.
		for k, s := range r.sessions {
			if persist.KeyHash(k) == keyhash {
				return s.Workspace()
			}
		}
		return ""
	}
}

// startAttachmentTracker spins up the tracker bound to r's
// eventLogDir + session table. Called from NewRouter AFTER the
// persister + session map are constructed so the resolver closure
// is ready to serve lookups.
//
// When the tracker fails to start (unusual — only happens if
// Options validation is wrong) we log + continue without tracking.
// Attachments then fall back to pure upload-TTL GC, so the service
// still works, just without the refcount retention bonus.
func (r *Router) startAttachmentTracker() {
	if r.eventLogDir == "" {
		// Refcount tracking only makes sense when we have the
		// event-log persistence tier emitting OnPersistedEntry
		// signals. Without it, the tracker would never bump.
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

// stopAttachmentTracker flushes pending bumps and releases the
// worker goroutine. Called from Router.shutdown AFTER the persister
// itself has stopped so no more OnPersistedEntry callbacks arrive
// while we're trying to drain.
//
// Intentionally parents on context.Background, NOT r.historyCtx:
// shutdown's first action cancels historyCtx, so deriving from it
// here would yield an already-cancelled context and the tracker
// drain loop would get zero time to flush pending bumps. R230-GO-5
// tracks splitting shutdown into a dedicated stop-ctx; until that
// lands the 5s budget is the load-bearing bound.
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
