package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/server"
)

// configReloader re-reads config.yaml on demand and applies the hot sections
// (docs/rfc/config-hot-reload.md §3.3). Built before the server so its
// Reload method can be handed to ServerOptions; the server's ApplyHotConfig
// is bound afterwards with bindApply, before any entry point can call Reload.
type configReloader struct {
	path string
	// baseline is the configuration the process started with; every
	// restart_required list is computed against it, so a difference the
	// process cannot absorb keeps being reported until a real restart.
	baseline *config.Config
	level    *slog.LevelVar
	fp       *server.ConfigFingerprint

	mu    sync.Mutex
	last  *config.Config // last successfully applied configuration
	apply func(server.HotConfig)
}

// errReloadNotBound is returned when Reload runs before bindApply.
var errReloadNotBound = errors.New("config reload: server not bound yet")

func newConfigReloader(path string, cfg *config.Config, level *slog.LevelVar, fp *server.ConfigFingerprint) *configReloader {
	return &configReloader{path: path, baseline: cfg, last: cfg, level: level, fp: fp}
}

// bindApply installs the function that applies a HotConfig to the running
// server.
func (r *configReloader) bindApply(apply func(server.HotConfig)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.apply = apply
}

// hotConfigOf derives the runtime values of the hot sections.
func hotConfigOf(cfg *config.Config) server.HotConfig {
	return server.HotConfig{
		Access: cfg.IMAccessPolicy(),
		IMLimits: server.IMLimitsOptions{
			Budget:            cfg.IMLimits.BudgetPolicy(),
			UserRatePerMinute: cfg.IMLimits.UserRate.PerMinute,
			UserRateBurst:     cfg.IMLimits.EffectiveBurst(),
		},
	}
}

// Reload loads the file, and on success applies its hot sections, updates
// the log level and the /health fingerprint. A file that fails to load leaves
// the process exactly as it was.
func (r *configReloader) Reload(ctx context.Context) (config.ReloadResult, error) {
	next, err := config.Load(r.path)
	if err != nil {
		slog.Warn("config reload failed; running config unchanged", "path", r.path, "err", err)
		return config.ReloadResult{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.apply == nil {
		return config.ReloadResult{}, errReloadNotBound
	}
	res := config.ReloadResult{
		SHA256:          next.Fingerprint.SHA256,
		LoadedAt:        next.Fingerprint.LoadedAt,
		Applied:         r.last.HotChanged(next),
		RestartRequired: r.baseline.RestartRequired(next),
	}
	if r.level != nil {
		r.level.Set(resolveLogLevel(next.Log.Level))
	}
	r.apply(hotConfigOf(next))
	r.fp.Set(next.Fingerprint.SHA256, next.Fingerprint.LoadedAt)
	r.last = next
	slog.Info("config reloaded", "path", r.path, "sha256", shortSHA(res.SHA256),
		"applied", res.Applied, "restart_required", res.RestartRequired)
	for _, p := range next.IMAccessPostures() {
		if p.Open {
			slog.Warn("im access: platform open to every sender after reload", "platform", p.Platform)
		}
	}
	return res, nil
}

// shortSHA abbreviates a hex digest for log lines; short inputs pass through.
func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
