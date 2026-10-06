package system

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/promexport"
	"github.com/naozhi/naozhi/internal/ratelimit"
	"golang.org/x/time/rate"
)

// ConfigReloader re-reads config.yaml and applies its hot sections
// (docs/rfc/config-hot-reload.md §3.4); nil means no file path is known and
// the endpoint answers 501.
type ConfigReloader func(ctx context.Context) (config.ReloadResult, error)

// configReloadTotal counts reload attempts by outcome ("ok" / "error").
var configReloadTotal = promexport.NewMap("naozhi_config_reload_total", "outcome")

// configReloadKey is the single bucket of the reload limiter.
const configReloadKey = "config-reload"

// newConfigReloadLimiter allows one reload per 10s: a reload re-parses the
// file and may swap the IM access policy and rebuild the rate-limit buckets,
// and nothing legitimate needs it faster.
func newConfigReloadLimiter() *ratelimit.Limiter {
	return ratelimit.New(ratelimit.Config{Rate: rate.Every(10 * time.Second), Burst: 1, MaxKeys: 4, TTL: time.Hour})
}

// HandleConfigReload serves POST /api/system/config/reload: 200 with the
// config.ReloadResult; 422 when the new file fails validation (the running
// config is untouched); 501 when no reloader is wired; 429 when throttled.
func (h *Handlers) HandleConfigReload(w http.ResponseWriter, r *http.Request) {
	if h.configReload == nil {
		http.Error(w, "config reload is not available (no config path)", http.StatusNotImplemented)
		return
	}
	if h.reloadLimiter != nil && !h.reloadLimiter.Allow(configReloadKey) {
		http.Error(w, "too many reload attempts; wait a moment", http.StatusTooManyRequests)
		return
	}
	res, err := h.configReload(r.Context())
	if err != nil {
		configReloadTotal.Add("error", 1)
		// Load errors quote file content (unknown keys, bad values).
		http.Error(w, "config reload failed: "+osutil.SanitizeForLog(err.Error(), 2048), http.StatusUnprocessableEntity)
		return
	}
	configReloadTotal.Add("ok", 1)
	slog.Info("config reloaded via api", "applied", res.Applied, "restart_required", res.RestartRequired)
	httputil.WriteJSON(w, res)
}
