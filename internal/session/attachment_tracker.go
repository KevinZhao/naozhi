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
// The closure iterates r.sessions once per call. Session removal is
// rare; hot-path bumps are infrequent enough (bounded by the
// Persister channel drain rate) that the linear scan is cheaper
// than maintaining a dedicated keyhash → workspace index.
func (r *Router) workspaceResolverForTracker() tracker.WorkspaceResolver {
	return func(keyhash string) string {
		if keyhash == "" {
			return ""
		}
		r.mu.RLock()
		defer r.mu.RUnlock()
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
