package server

import (
	"net/http"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// CLIBackendsHandler serves the read-only CLI-backends list the dashboard
// consumes when rendering the "new session" picker.
//
// `detected` is probed once at construction (each probe invokes a 5s
// subprocess timeout per backend binary). Without caching, every call to
// /api/cli/backends would block the HTTP goroutine up to 5s×N — an
// authenticated user could fork-storm by polling this endpoint.
type CLIBackendsHandler struct {
	router   *session.Router
	detected []cli.BackendInfo // pre-computed at startup, immutable after
}

// NewCLIBackendsHandler pre-computes the expensive backend probe so the HTTP
// handler can respond in O(enabled backends) time without spawning
// subprocesses on each request.
func NewCLIBackendsHandler(router *session.Router) *CLIBackendsHandler {
	detected := cli.DetectBackends()
	cli.SortBackendsAvailableFirst(detected)
	// Redact Path: revealing installed-binary paths to any authenticated
	// dashboard user leaks host filesystem layout and aids post-XSS
	// privilege escalation. The dashboard UI only needs ID/availability.
	for i := range detected {
		detected[i].Path = ""
	}
	return &CLIBackendsHandler{router: router, detected: detected}
}

// response shape: {"backends": [...], "default": "claude", "detected": [...]}.
//
// `backends` lists the backends this naozhi instance is configured to spawn
// (one Router entry per enabled backend), each annotated with whatever CLI
// metadata the matching wrapper collected at startup.
//
// `detected` lists every backend naozhi knows how to drive, including ones
// NOT enabled in config — exposed so an operator can see "kiro-cli is
// installed but not configured" from the UI without grepping logs.
func (h *CLIBackendsHandler) handle(w http.ResponseWriter, r *http.Request) {
	defaultID := h.router.DefaultBackend()

	ids := h.router.BackendIDs()
	backends := make([]cli.BackendInfo, 0, len(ids))
	for _, id := range ids {
		info := cli.BackendInfo{ID: id, Available: true}
		if wr := h.router.BackendWrapper(id); wr != nil {
			info.DisplayName = wr.CLIName
			// Path intentionally omitted — see NewCLIBackendsHandler comment.
			info.Version = wr.CLIVersion
			if wr.Protocol != nil {
				info.Protocol = wr.Protocol.Name()
			}
			// Version==""（二进制存在但 --version 解析失败）不应冒充
			// Available=true —— dashboard 会以 disabled 样式显示。
			info.Available = wr.CLIVersion != ""
		}
		backends = append(backends, info)
	}

	writeJSON(w, map[string]any{
		"backends": backends,
		"default":  defaultID,
		"detected": h.detected,
	})
}
