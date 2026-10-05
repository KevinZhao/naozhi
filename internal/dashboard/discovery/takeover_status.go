package discovery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/session"
)

const (
	takeoverOutcomeTTL = 2 * time.Minute
	takeoverOutcomeCap = 64
)

// Takeover attempt states GET /api/discovered/takeover/status reports.
const (
	takeoverPending = "pending"
	takeoverReady   = "ready"
	takeoverFailed  = "failed"
	takeoverUnknown = "unknown"
)

type takeoverOutcome struct {
	State string `json:"state"`
	Class string `json:"class,omitempty"`
	at    time.Time
}

// takeoverTracker holds the outcome of recent takeover attempts by a random
// per-attempt ID, so the dashboard learns about a failure that only happens
// after HandleTakeover has answered 202. Entries expire after
// takeoverOutcomeTTL; at most takeoverOutcomeCap are kept, oldest dropped.
type takeoverTracker struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[string]takeoverOutcome
}

func newTakeoverTracker() *takeoverTracker {
	return &takeoverTracker{now: time.Now, m: make(map[string]takeoverOutcome)}
}

// begin records a pending attempt and returns its ID: 32 hex characters.
func (t *takeoverTracker) begin() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand, Go 1.24+)
	id := hex.EncodeToString(b[:])
	t.record(id, takeoverOutcome{State: takeoverPending})
	return id
}

// finish records how the attempt ended; err == nil means the session is up.
func (t *takeoverTracker) finish(id string, err error) {
	if err == nil {
		t.record(id, takeoverOutcome{State: takeoverReady})
		return
	}
	t.record(id, takeoverOutcome{State: takeoverFailed, Class: classifyTakeoverErr(err)})
}

func (t *takeoverTracker) record(id string, o takeoverOutcome) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	o.at = now
	t.m[id] = o
	var oldest string
	for k, v := range t.m {
		if now.Sub(v.at) > takeoverOutcomeTTL {
			delete(t.m, k)
		} else if oldest == "" || v.at.Before(t.m[oldest].at) {
			oldest = k
		}
	}
	if len(t.m) > takeoverOutcomeCap {
		delete(t.m, oldest)
	}
}

func (t *takeoverTracker) lookup(id string) takeoverOutcome {
	t.mu.Lock()
	defer t.mu.Unlock()
	o, ok := t.m[id]
	if !ok || t.now().Sub(o.at) > takeoverOutcomeTTL {
		return takeoverOutcome{State: takeoverUnknown}
	}
	return o
}

// classifyTakeoverErr maps a background Takeover error to the class the
// dashboard turns into copy. The error text itself stays in the log: it can
// carry paths and session keys.
func classifyTakeoverErr(err error) string {
	switch {
	case errors.Is(err, session.ErrMaxProcs):
		return "max_procs"
	case errors.Is(err, session.ErrSpawnInFlight), errors.Is(err, session.ErrTakeoverRaced):
		return "in_progress"
	case errors.Is(err, session.ErrRouterStopped), errors.Is(err, context.Canceled):
		return "shutting_down"
	case errors.Is(err, session.ErrCLIStartupFailed):
		return "startup_failed"
	case errors.Is(err, session.ErrShimStuck):
		return "shim_stuck"
	default:
		return "spawn_failed"
	}
}

func validTakeoverID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// HandleTakeoverStatus serves GET /api/discovered/takeover/status?id= — the
// outcome of a local takeover HandleTakeover accepted. An expired or never
// issued ID reports "unknown".
func (h *Handlers) HandleTakeoverStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if !validTakeoverID(id) {
		http.Error(w, "invalid takeover id", http.StatusBadRequest)
		return
	}
	httputil.WriteJSON(w, h.takeovers.lookup(id))
}
