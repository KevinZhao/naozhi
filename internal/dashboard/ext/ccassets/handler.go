package ccassets

import (
	"errors"
	"net/http"

	"github.com/naozhi/naozhi/internal/assets"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
)

// Handler serves the read-only asset-browser endpoints.
type Handler struct {
	providers  map[string]assets.Provider
	home       string
	repoRootFn func(*http.Request) string
	limiter    IPLimiter
}

// New constructs a Handler; repoRootFn may return "" (no workspace root).
func New(providers map[string]assets.Provider, home string, repoRootFn func(*http.Request) string, limiter IPLimiter) *Handler {
	if repoRootFn == nil {
		repoRootFn = func(*http.Request) string { return "" }
	}
	return &Handler{providers: providers, home: home, repoRootFn: repoRootFn, limiter: limiter}
}

// providerFor resolves ?backend= to a provider; empty defaults to the sole
// registered provider, else "claude". Returns nil if not found.
func (h *Handler) providerFor(r *http.Request) (assets.Provider, string) {
	id := r.URL.Query().Get("backend")
	if id == "" {
		if len(h.providers) == 1 {
			for k, p := range h.providers {
				return p, k
			}
		}
		id = "claude"
	}
	return h.providers[id], id
}

// HandleList serves GET /api/cc/assets. Returns the full Inventory, optionally
// sliced to ?kind=. Totals always reflects the full scan (D4/D5).
func (h *Handler) HandleList(w http.ResponseWriter, r *http.Request) {
	if !h.limiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
		return
	}
	prov, _ := h.providerFor(r)
	if prov == nil {
		// No provider: empty inventory, not 404, so the frontend hides uniformly.
		httputil.WriteJSON(w, &assets.Inventory{Totals: map[string]int{}})
		return
	}
	inv, err := prov.Scan(assets.ScanRequest{
		Home:     h.home,
		RepoRoot: h.repoRootFn(r),
		Kind:     r.URL.Query().Get("kind"),
	})
	if err != nil {
		httputil.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "scan_failed"})
		return
	}
	httputil.WriteJSON(w, inv)
}

// HandleRaw serves GET /api/cc/assets/raw: raw asset bytes as text/plain.
func (h *Handler) HandleRaw(w http.ResponseWriter, r *http.Request) {
	if !h.limiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
		return
	}
	prov, _ := h.providerFor(r)
	if prov == nil {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "no_backend"})
		return
	}
	q := r.URL.Query()
	ref := assets.Ref{
		Kind: q.Get("kind"),
		Source: assets.Source{
			Kind:    q.Get("source"),
			Plugin:  q.Get("plugin"),
			Project: q.Get("project"),
		},
		RelPath: q.Get("rel"),
		Anchor:  q.Get("anchor"),
	}
	raw, err := prov.ReadRaw(assets.RawRequest{
		Home:     h.home,
		RepoRoot: h.repoRootFn(r),
		Ref:      ref,
	})
	if err != nil {
		status, msg := classifyRawErr(err)
		httputil.WriteJSONStatus(w, status, map[string]string{"error": msg})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Force download: operator-installed plugin assets are untrusted content and
	// must never render inline (<iframe src>). XHR/fetch body reads are unaffected.
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(raw)
}

// classifyRawErr maps ReadRaw sentinels to HTTP status. Not-found and
// path-escape both surface as 404 (don't leak existence); oversize is 413.
func classifyRawErr(err error) (int, string) {
	switch {
	case errors.Is(err, assets.ErrTooLarge):
		return http.StatusRequestEntityTooLarge, "too_large"
	case errors.Is(err, assets.ErrNotFound):
		return http.StatusNotFound, "not_found"
	default:
		return http.StatusInternalServerError, "read_failed"
	}
}
