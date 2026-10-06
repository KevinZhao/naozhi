package server

import (
	"log/slog"
	"net/http"

	"github.com/naozhi/naozhi/internal/promexport"
)

// registerMetrics wires GET /metrics: the naozhi_* expvar counters in
// Prometheus text format (docs/ops/metrics.md). Unlike /api/debug/vars it is
// not loopback-only — a scraper lives on another host — but it keeps the
// dashboard-token gate and refuses to run without one, since the counters
// (message volumes, denials, spend blocks) describe the deployment. GET-only
// and non-mutating, so RequireSameOrigin is not needed (see RequireAuth's
// doc); Bearer is the expected credential, the cookie also works.
func (s *Server) registerMetrics() {
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.dashboardToken == "" {
		http.Error(w, "metrics disabled: set a dashboard token to enable", http.StatusForbidden)
		return
	}
	if !s.auth.IsAuthenticated(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="naozhi"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", promexport.ContentType)
	if err := promexport.Write(w); err != nil {
		slog.Debug("metrics write", "err", err)
	}
}
