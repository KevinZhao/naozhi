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

// NodeAccessor is the subset of internal/server.NodeAccessor this handler
// uses to proxy /api/cli/backends?node=<id> to a remote node. server's
// *nodeAccessor satisfies this shape; we accept the interface so the
// sub-package doesn't reverse-import server (mirrors dashboard/session's
// NodeAccessor). Nil is allowed — single-node deployments never set it and
// the ?node= branch stays unreachable.
type NodeAccessor interface {
	LookupNode(w http.ResponseWriter, id string) (node.Conn, bool)
}

// Handler serves the read-only CLI-backends list the dashboard
// consumes when rendering the "new session" picker.
//
// `detected` is probed once at construction (each probe invokes a 5s
// subprocess timeout per backend binary). Without caching, every call to
// /api/cli/backends would block the HTTP goroutine up to 5s×N — an
// authenticated user could fork-storm by polling this endpoint.
//
// nodeAccess is optional: when the dashboard's node picker targets a remote
// node, Handle proxies the manifest request to that node so the picker
// renders the REMOTE node's backends (and its default), not the primary's.
// Without this, a multi-node dashboard creating a session on a remote node
// pre-selected the primary's default backend — the picker node-aware fix.
type Handler struct {
	router     *session.Router
	detected   []clipkg.BackendInfo // pre-computed at startup, immutable after
	nodeAccess NodeAccessor         // nil in single-node / test deployments
}

// NewCLIBackendsHandler pre-computes the expensive backend probe so the HTTP
// handler can respond in O(enabled backends) time without spawning
// subprocesses on each request. Uses context.Background() for the probe —
// prefer NewCLIBackendsHandlerCtx when the caller has a shutdown context.
//
// Deprecated: prefer NewCLIBackendsHandlerCtx.
func NewCLIBackendsHandler(router *session.Router) *Handler {
	return NewCLIBackendsHandlerCtx(context.Background(), router)
}

// NewCLIBackendsHandlerCtx is the context-aware variant of
// NewCLIBackendsHandler. The ctx is threaded into DetectBackendsCtx so
// SIGTERM during startup aborts the --version probe promptly instead of
// waiting 5s×N. R55-QUAL-004.
func NewCLIBackendsHandlerCtx(ctx context.Context, router *session.Router) *Handler {
	detected := clipkg.DetectBackendsCtx(ctx)
	clipkg.SortBackendsAvailableFirst(detected)
	// Redact Path and Version: revealing installed-binary paths to any
	// authenticated dashboard user leaks host filesystem layout, and CLI
	// versions of backends NOT enabled in naozhi config fingerprint
	// host software for secondary exploitation (known CVE targeting).
	// The dashboard UI for `detected` only needs id+available to render
	// "installed but unconfigured" — version adds no user-facing value.
	for i := range detected {
		detected[i].Path = ""
		detected[i].Version = ""
	}
	return &Handler{router: router, detected: detected}
}

// SetNodeAccess wires the node accessor used to proxy ?node=<id> requests
// to a remote node. Called once at server wiring time (single-threaded, no
// concurrent Handle in flight yet). Passing nil is a no-op that leaves the
// handler local-only.
func (h *Handler) SetNodeAccess(na NodeAccessor) { h.nodeAccess = na }

// response shape: {"backends": [...], "default": "claude", "detected": [...]}.
//
// `backends` lists the backends this naozhi instance is configured to spawn
// (one Router entry per enabled backend), each annotated with whatever CLI
// metadata the matching wrapper collected at startup.
//
// `detected` lists every backend naozhi knows how to drive, including ones
// NOT enabled in config — exposed so an operator can see "kiro-cli is
// installed but not configured" from the UI without grepping logs.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) {
	// Remote node proxy — the node picker targets a remote node, so the
	// picker must render THAT node's backends + default, not ours. Mirrors
	// the ?node= proxy in dashboard/session HandleEvents. Empty / "local"
	// falls through to the local manifest below.
	if nodeID := r.URL.Query().Get("node"); nodeID != "" && nodeID != "local" {
		if h.nodeAccess == nil {
			// Multi-node not wired (single-node build): the picker should
			// never send ?node= here, but degrade cleanly rather than 500.
			http.Error(w, "node routing not available", http.StatusBadGateway)
			return
		}
		nc, ok := h.nodeAccess.LookupNode(w, nodeID)
		if !ok {
			return // LookupNode already wrote the error response.
		}
		raw, err := nc.FetchBackends(r.Context())
		if err != nil {
			// Older peer binaries predate fetch_backends: the reverse-RPC
			// path returns an error and the HTTP path returns non-200. Either
			// way the dashboard's fetch throws and the picker collapses to the
			// single-backend UI (renderBackendPicker(null) → ''), so a 502
			// here is the honest signal (not a silent wrong list).
			slog.Warn("remote fetch backends failed", "node", nodeID, "err", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		httputil.WriteJSONRaw(w, raw)
		return
	}

	// Local manifest — assembled by session.Router so the reverse-RPC
	// "fetch_backends" branch renders an identical shape (single source of
	// truth; see router_backend_manifest.go).
	httputil.WriteJSON(w, h.router.BackendsManifest(h.detected))
}
