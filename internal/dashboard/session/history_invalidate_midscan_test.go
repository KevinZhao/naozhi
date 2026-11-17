package session

import (
	"context"
	"testing"
)

// retiringRouter retires a session while a history scan is under way: the
// scan's exclude snapshot still lists it as live, and the retirement's
// InvalidateHistoryCache lands before the scan installs its result.
type retiringRouter struct {
	*fakeRouter
	h *Handlers
}

func (r retiringRouter) DiscoveryExcludeIDs() map[string]bool {
	ids := map[string]bool{"just-retired": true}
	r.h.InvalidateHistoryCache()
	return ids
}

// The invalidation promised the next poll a list with the retired session;
// a scan that finished after it, from the older snapshot, must not mark the
// cache fresh and hold that list back for the 120s TTL.
func TestLoadHistorySessionsCtx_InvalidateDuringScanForcesRescan(t *testing.T) {
	h := &Handlers{deps: Deps{ClaudeDir: t.TempDir()}}
	h.deps.Router = retiringRouter{fakeRouter: newFakeRouter(), h: h}

	h.loadHistorySessionsCtx(context.Background())
	if ns := h.historyCacheTimeUnixNano.Load(); ns != 0 {
		t.Fatal("scan overlapping an invalidation marked the cache fresh; the next poll serves the stale list")
	}

	// Without an invalidation during the scan the result is cached as before.
	h.deps.Router = newFakeRouter()
	h.loadHistorySessionsCtx(context.Background())
	if h.historyCacheTimeUnixNano.Load() == 0 {
		t.Error("an undisturbed scan left the cache stale")
	}
}
