package ccassets

import (
	"errors"
	"net/http"

	"github.com/naozhi/naozhi/internal/assets"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
)

// Handler serves the read-only asset-browser endpoints. It holds a map of
// backend id -> provider (only backends that expose a provider), the resolved
// Claude home dir, a per-request repoRoot resolver, and a rate limiter.
type Handler struct {
	providers  map[string]assets.Provider
	home       string
	repoRootFn func(*http.Request) string
	limiter    IPLimiter
}

// New constructs a Handler. providers maps backend id -> provider; home is the
// resolved ~/.claude dir; repoRootFn resolves the current workspace root per
// request (may return "" — RFC §9.3); limiter rate-limits all endpoints.
func New(providers map[string]assets.Provider, home string, repoRootFn func(*http.Request) string, limiter IPLimiter) *Handler {
	if repoRootFn == nil {
		repoRootFn = func(*http.Request) string { return "" }
	}
	return &Handler{providers: providers, home: home, repoRootFn: repoRootFn, limiter: limiter}
}

// providerFor resolves the backend query param to a provider. Empty backend
// defaults to the sole provider when exactly one is registered (first phase:
// only claude). Returns nil if not found.
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
		// No provider for this backend: empty inventory, not 404, so the
		// frontend can uniformly hide the entry on an empty list.
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

// HandleRaw serves GET /api/cc/assets/raw. Returns the raw file bytes of the
// asset addressed by the Ref query params, as text/plain.
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
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(raw)
}

// classifyRawErr maps a ReadRaw error to an HTTP status using the sentinels
// exported from the assets leaf package (both sides import it, so no coupling
// to the concrete provider package). Not-found and path-escape both surface as
// 404 — don't leak whether the target exists; oversize is 413; else 500.
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
