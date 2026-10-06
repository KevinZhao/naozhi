package main

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/server"
	"github.com/naozhi/naozhi/internal/session"
)

// configReloader re-reads config.yaml on demand and applies the hot sections
// (docs/rfc/config-hot-reload.md §3.3). Built before the server so its
// Reload method can be handed to ServerOptions; the server's ApplyHotConfig
// is bound afterwards with bindApply, before any entry point can call Reload.
type configReloader struct {
	path string
	// baseline is the configuration the process started with; every
	// restart_required list is computed against it (see running), so a
	// difference the process cannot absorb keeps being reported until a
	// real restart.
	baseline *config.Config
	level    *slog.LevelVar
	fp       *server.ConfigFingerprint
	load     func(path string) (*config.Config, error) // config.Load; tests observe the read

	mu    sync.Mutex
	last  *config.Config // last successfully applied configuration
	apply func(server.HotConfig)
	// liveProfiles reads the access-profile registry, which the dashboard
	// grows at runtime; nil compares access_profiles against baseline alone.
	liveProfiles func() map[string]session.AccessProfile
}

// errReloadNotBound is returned when Reload runs before bindApply.
var errReloadNotBound = errors.New("config reload: server not bound yet")

func newConfigReloader(path string, cfg *config.Config, level *slog.LevelVar, fp *server.ConfigFingerprint) *configReloader {
	return &configReloader{path: path, baseline: cfg, last: cfg, level: level, fp: fp, load: config.Load}
}

// bindApply installs the function that applies a HotConfig to the running
// server and the reader of the live access-profile registry.
func (r *configReloader) bindApply(apply func(server.HotConfig), liveProfiles func() map[string]session.AccessProfile) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.apply = apply
	r.liveProfiles = liveProfiles
}

// running is baseline plus the access profiles created at runtime (the
// dashboard writes them to config.yaml and registers them live), so a reload
// does not list as pending a profile that is already in force.
func (r *configReloader) running() *config.Config {
	if r.liveProfiles == nil {
		return r.baseline
	}
	var profiles map[string]config.AccessProfile
	for id, ap := range r.liveProfiles() {
		if _, ok := r.baseline.AccessProfiles[id]; ok {
			continue
		}
		if profiles == nil {
			profiles = maps.Clone(r.baseline.AccessProfiles)
			if profiles == nil {
				profiles = map[string]config.AccessProfile{}
			}
		}
		profiles[id] = configAccessProfile(ap)
	}
	if profiles == nil {
		return r.baseline
	}
	run := *r.baseline
	run.AccessProfiles = profiles
	return &run
}

// configAccessProfile is the config form a runtime-created profile loads back
// as: AppendAccessProfile writes no env block for an empty Env.
func configAccessProfile(ap session.AccessProfile) config.AccessProfile {
	out := config.AccessProfile{
		DisplayName:    ap.DisplayName,
		ChipColor:      ap.ChipColor,
		DefaultModel:   ap.DefaultModel,
		DefaultBackend: ap.DefaultBackend,
	}
	if len(ap.Env) > 0 {
		out.Env = ap.Env
	}
	return out
}

// hotConfigOf derives the runtime values of the hot sections; sections names
// the ones to apply.
func hotConfigOf(cfg *config.Config, sections []string) server.HotConfig {
	return server.HotConfig{
		Access:    cfg.IMAccessPolicy(),
		RateLimit: dispatch.RateLimit{MsgsPerMin: cfg.IMRateLimit.MsgsPerMin, Burst: cfg.IMRateLimit.Burst},
		Sections:  sections,
	}
}

// Reload loads the file, and on success applies the hot sections that
// changed, updates the log level and the /health fingerprint. A file that
// fails to load leaves the process exactly as it was. The lock covers the
// read, so of two overlapping reloads the later one's file is what stays.
func (r *configReloader) Reload(ctx context.Context) (config.ReloadResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.apply == nil {
		return config.ReloadResult{}, errReloadNotBound
	}
	next, err := r.load(r.path)
	if err != nil {
		slog.Warn("config reload failed; running config unchanged", "path", r.path, "err", err)
		return config.ReloadResult{}, err
	}
	res := config.ReloadResult{
		SHA256:          next.Fingerprint.SHA256,
		LoadedAt:        next.Fingerprint.LoadedAt,
		Applied:         r.last.HotChanged(next),
		RestartRequired: r.running().RestartRequired(next),
		OpenedPlatforms: r.baseline.IMAccessOpened(r.last, next),
		OpenPlatforms:   r.baseline.IMAccessOpen(next),
	}
	if r.level != nil {
		r.level.Set(resolveLogLevel(next.Log.Level))
	}
	if len(res.Applied) > 0 {
		r.apply(hotConfigOf(next, res.Applied))
	}
	r.fp.Set(next.Fingerprint.SHA256, next.Fingerprint.LoadedAt, res.RestartRequired)
	r.last = next
	slog.Info("config reloaded", "path", r.path, "sha256", shortSHA(res.SHA256),
		"applied", res.Applied, "restart_required", res.RestartRequired)
	for _, name := range res.OpenedPlatforms {
		slog.Error("im access: reload OPENED a restricted platform to every sender; check the im_access key spelling",
			"platform", name)
	}
	for _, name := range res.OpenPlatforms {
		if !slices.Contains(res.OpenedPlatforms, name) {
			slog.Warn("im access: platform open to every sender after reload", "platform", name)
		}
	}
	return res, nil
}

// watchSignals routes the process signals: SIGHUP calls reload and keeps
// running (docs/rfc/config-hot-reload.md §3.4), SIGTERM / SIGINT call
// shutdown. stop unregisters, for tests.
func watchSignals(reload func(), shutdown func(reason string)) (stop func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go signalLoop(ch, reload, shutdown)
	return func() {
		signal.Stop(ch)
		close(ch)
	}
}

// signalLoop handles signals until the first one that is not SIGHUP, which
// gets shutdown("signal:<name>"); a bad file on SIGHUP is reload's to log.
func signalLoop(ch <-chan os.Signal, reload func(), shutdown func(reason string)) {
	for sig := range ch {
		if sig == syscall.SIGHUP {
			reload()
			continue
		}
		shutdown("signal:" + sig.String())
		return
	}
}

// shortSHA abbreviates a hex digest for log lines; short inputs pass through.
func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
