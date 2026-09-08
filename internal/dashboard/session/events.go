package session

import (
	"log/slog"
	"net/http"
	"strconv"

	sessionpkg "github.com/naozhi/naozhi/internal/session"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
)

// eventsBefore returns the entries strictly older than the `before` cursor
// (unix ms), preserving the input's chronological order. Always returns a
// non-nil slice so an exhausted page serialises as [] rather than null.
func eventsBefore(entries []clievent.EventEntry, before int64) []clievent.EventEntry {
	out := make([]clievent.EventEntry, 0, len(entries))
	for _, e := range entries {
		if e.Time < before {
			out = append(out, e)
		}
	}
	return out
}

// HandleEvents serves GET /api/sessions/events?key=&node=&after=&before=&limit=.
// `after` (ms) is an incremental fetch with Time >= after (watermark re-admitted,
// #2456; client dedups by uuid); `before` (ms) pages strictly older entries,
// newest `limit` of them in chronological order; `limit` alone sizes the
// initial page. `after` wins over `before`; no params returns full history.
func (h *Handlers) HandleEvents(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing key parameter", http.StatusBadRequest)
		return
	}
	// Same gate as the reverse-RPC fetch_events handler: rejects multi-KB or
	// control-byte keys before they reach slog attrs; also caps length at
	// MaxSessionKeyBytes.
	if err := sessionpkg.ValidateSessionKey(key); err != nil {
		http.Error(w, "invalid key parameter", http.StatusBadRequest)
		return
	}

	q := r.URL.Query()
	afterStr := q.Get("after")
	beforeStr := q.Get("before")
	limitStr := q.Get("limit")

	var (
		after  int64
		before int64
		limit  int
	)
	if afterStr != "" {
		v, err := strconv.ParseInt(afterStr, 10, 64)
		if err != nil {
			http.Error(w, "invalid after parameter", http.StatusBadRequest)
			return
		}
		after = v
	}
	if beforeStr != "" {
		v, err := strconv.ParseInt(beforeStr, 10, 64)
		if err != nil {
			http.Error(w, "invalid before parameter", http.StatusBadRequest)
			return
		}
		before = v
	}
	if limitStr != "" {
		v, err := strconv.Atoi(limitStr)
		if err != nil || v < 0 {
			http.Error(w, "invalid limit parameter", http.StatusBadRequest)
			return
		}
		if v > maxEventsPageLimit {
			v = maxEventsPageLimit
		}
		limit = v
	}

	// Remote node proxy — the node RPC only carries `after`, so `before` /
	// `limit` pagination is emulated locally; older peers keep working.
	nodeID := q.Get("node")
	if nodeID != "" && nodeID != "local" {
		nc, ok := h.nodeAccess.LookupNode(w, nodeID)
		if !ok {
			return
		}
		entries, err := nc.FetchEvents(r.Context(), key, after)
		if err != nil {
			slog.Warn("remote fetch events failed", "node", nodeID, "key", key, "err", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		if beforeStr != "" && afterStr == "" {
			// "Load earlier" page (#2433): strictly older than the cursor, newest
			// `limit` of those, plus an authoritative has-more flag. An empty page
			// is the client's stop signal, so it must be [] with has-more=0.
			pageLimit := limit
			if pageLimit == 0 {
				pageLimit = maxEventsPageLimit
			}
			page := eventsBefore(entries, before)
			hasMore := len(page) > pageLimit
			if hasMore {
				page = page[len(page)-pageLimit:]
			}
			if hasMore {
				w.Header().Set("X-Events-Has-More", "1")
			} else {
				w.Header().Set("X-Events-Has-More", "0")
			}
			httputil.WriteJSON(w, page)
			return
		}
		// Page cap so legacy peers still yield a consistent-size payload.
		if limit > 0 && len(entries) > limit {
			entries = entries[len(entries)-limit:]
		}
		httputil.WriteJSON(w, entries)
		return
	}

	// Local
	sess := h.router.SessionFor(key)
	if sess == nil && h.scheduler != nil && h.scheduler.EnsureStub(key) {
		// Cron stubs torn down by sidebar "×" are lazily rebuilt on next click so
		// polling (WS-down) clients don't get a permanent 404 until the next tick.
		sess = h.router.SessionFor(key)
	}
	if sess == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	var entries []clievent.EventEntry
	switch {
	case afterStr != "":
		entries = sess.EventEntriesSince(cli.SinceInclusive(after))
		if limit > 0 && len(entries) > limit {
			// Preserve the newest on a full catch-up so the client doesn't
			// miss events it just streamed through.
			entries = entries[len(entries)-limit:]
		}
	case beforeStr == "" && limit > 0:
		// Initial page (limit only): visible-aware read mirroring the WS
		// subscribe handshake, so a tail-N of internal-only events (agent team)
		// doesn't render the blank "该会话最近仅有 agent 活动" placeholder.
		visTarget := limit
		if visTarget > sessionpkg.DefaultVisibleTarget {
			visTarget = sessionpkg.DefaultVisibleTarget
		}
		// maxTotal=0 lets the reader use its own ceiling (ring size) so visible
		// bubbles beyond `limit` under an internal flood still surface.
		// X-Events-Has-More mirrors the WS "history" has_more field; it rides a
		// header so the bare-array body contract holds. Always set ("0"/"1") on
		// this branch; an absent header means legacy server / remote relay.
		var hasMore bool
		entries, hasMore = sess.EventInitialPageCtx(r.Context(), visTarget, 0)
		if hasMore {
			w.Header().Set("X-Events-Has-More", "1")
		} else {
			w.Header().Set("X-Events-Has-More", "0")
		}
	case beforeStr != "":
		pageLimit := limit
		if pageLimit == 0 {
			pageLimit = maxEventsPageLimit
		}
		// "Load earlier": a plain time-ordered page — the visible-aware reader
		// would skip internal events the operator is paging toward.
		// EventEntriesBeforeCtx falls back to the backend's history.Source
		// (JSONL for claude) when memory no longer holds entries older than
		// `before`; the request ctx lets a cancelled fetch unblock disk I/O.
		entries = sess.EventEntriesBeforeCtx(r.Context(), before, pageLimit)
	default:
		entries = sess.EventEntries()
	}

	httputil.WriteJSON(w, entries)
}
