package cli

import (
	"context"
	"log/slog"
	"net/http"

	clipkg "github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// NodeAccessor is the 1-method subset of contracts.NodeAccessor used to proxy
// /api/cli/backends?node=<id>; kept narrow for the test doubles and asserted a
// strict subset by TestDashboardContractsDeclaredOnce (#2285). Nil in
// single-node deployments.
type NodeAccessor interface {
	LookupNode(w http.ResponseWriter, id string) (node.Conn, bool)
}

// Handler serves the read-only CLI-backends list behind the dashboard
// "new session" picker.
//
// `detected` is probed once at construction (5s subprocess timeout per
// backend); probing per request would let an authenticated user fork-storm
// by polling. nodeAccess is optional: with a remote node selected, Handle
// proxies so the picker renders THAT node's backends and default.
type Handler struct {
	router     *session.Router
	detected   []clipkg.BackendInfo // pre-computed at startup, immutable after
	nodeAccess NodeAccessor         // nil in single-node / test deployments
}

// NewCLIBackendsHandler pre-computes the backend probe with context.Background().
//
// Deprecated: prefer NewCLIBackendsHandlerCtx.
func NewCLIBackendsHandler(router *session.Router) *Handler {
	return NewCLIBackendsHandlerCtx(context.Background(), router)
}

// NewCLIBackendsHandlerCtx threads ctx into DetectBackendsCtx so SIGTERM
// during startup aborts the --version probe instead of waiting 5s×N.
func NewCLIBackendsHandlerCtx(ctx context.Context, router *session.Router) *Handler {
	detected := clipkg.DetectBackendsCtx(ctx)
	clipkg.SortBackendsAvailableFirst(detected)
	// Redact Path and Version: binary paths leak host filesystem layout and
	// versions of backends NOT enabled in config fingerprint host software.
	// The UI only needs id+available for "installed but unconfigured".
	for i := range detected {
		detected[i].Path = ""
		detected[i].Version = ""
	}
	return &Handler{router: router, detected: detected}
}

// SetNodeAccess wires the node accessor for ?node=<id> proxying. Called once
// at wiring time before any Handle is in flight; nil leaves the handler local-only.
func (h *Handler) SetNodeAccess(na NodeAccessor) { h.nodeAccess = na }

// Handle responds {"backends": [...], "default": "claude", "detected": [...]}:
// enabled Router entries with CLI metadata, plus every backend naozhi can
// drive (even unconfigured) so operators see "installed but not configured".
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) {
	// Remote node proxy: render THAT node's backends + default, mirroring the
	// ?node= proxy in session HandleEvents. Empty / "local" falls through.
	if nodeID := r.URL.Query().Get("node"); nodeID != "" && nodeID != "local" {
		if h.nodeAccess == nil {
			// Multi-node not wired: degrade cleanly rather than 500.
			http.Error(w, "node routing not available", http.StatusBadGateway)
			return
		}
		nc, ok := h.nodeAccess.LookupNode(w, nodeID)
		if !ok {
			return // LookupNode already wrote the error response.
		}
		raw, err := nc.FetchBackends(r.Context())
		if err != nil {
			// Older peers predate fetch_backends; 502 is the honest signal to the picker.
			slog.Warn("remote fetch backends failed", "node", nodeID, "err", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		httputil.WriteJSONRaw(w, raw)
		return
	}

	// Local manifest assembled by session.Router so the reverse-RPC
	// "fetch_backends" branch renders an identical shape.
	httputil.WriteJSON(w, h.router.BackendsManifest(h.detected))
}
