package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/ctxutil"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/server"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/shim"
	"github.com/naozhi/naozhi/internal/upstream"
	"github.com/naozhi/naozhi/internal/wireup"
)

// Pure init helpers extracted from main() so each is unit-testable against a
// fake config / shim manager (#396).

// resolveLogLevel maps a config.Log.Level string to a slog.Level; unknown or
// empty values fall back to Info.
func resolveLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// newLogHandler builds the slog.Handler for the configured format and level:
// "text" selects a TextHandler, anything else (incl. default "json") a JSONHandler.
func newLogHandler(w *os.File, cfg *config.Config, level slog.Leveler) slog.Handler {
	if level == nil {
		level = resolveLogLevel(cfg.Log.Level)
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if cfg.Log.Format == "text" {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	// trace_id / run_id / session_key from the ctx on every *Context log (#3436).
	return ctxutil.NewHandler(h)
}

// setupLogging installs the process-global slog default logger from cfg,
// writing to stdout. The returned LevelVar is what a config reload adjusts.
func setupLogging(cfg *config.Config) *slog.LevelVar {
	level := new(slog.LevelVar)
	level.Set(resolveLogLevel(cfg.Log.Level))
	slog.SetDefault(slog.New(newLogHandler(os.Stdout, cfg, level)))
	return level
}

// startWatchdogLoop launches the systemd liveness heartbeat goroutine.
// WATCHDOG=1 is sent unconditionally every 30s; the router HealthCheck result
// is a diagnostic only and never suppresses the heartbeat — normal write-lock
// activity (cleanup, spawn) would otherwise cause false negatives.
func startWatchdogLoop(ctx context.Context, hc func() bool) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if hc != nil && !hc() {
					slog.Warn("router mutex contended at watchdog tick")
				}
				_ = osutil.SdNotify("WATCHDOG=1")
			}
		}
	}()
}

// buildRemoteNodes constructs the multi-node aggregation client map from
// cfg.Nodes; nil when none are configured (server treats nil and empty alike).
func buildRemoteNodes(cfg *config.Config) map[string]node.Conn {
	if len(cfg.Nodes) == 0 {
		return nil
	}
	nodes := make(map[string]node.Conn, len(cfg.Nodes))
	for id, nc := range cfg.Nodes {
		nodes[id] = node.NewHTTPClient(id, nc.URL, nc.Token, nc.DisplayName)
	}
	return nodes
}

// buildReverseNodeAuth translates cfg.ReverseNodes into node.ReverseNodeAuth
// so internal/node does not import internal/config (#1411). Nil when no
// reverse nodes are configured, keeping the caller's len()>0 guard meaningful.
func buildReverseNodeAuth(cfg *config.Config) map[string]node.ReverseNodeAuth {
	if len(cfg.ReverseNodes) == 0 {
		return nil
	}
	auth := make(map[string]node.ReverseNodeAuth, len(cfg.ReverseNodes))
	for id, e := range cfg.ReverseNodes {
		auth[id] = node.ReverseNodeAuth{Token: e.Token, DisplayName: e.DisplayName}
	}
	return auth
}

// buildUpstreamConfig translates config.UpstreamConfig into upstream.Config so
// internal/upstream does not import internal/config (#1411). Nil when
// cfg.Upstream is nil.
func buildUpstreamConfig(cfg *config.Config) *upstream.Config {
	if cfg.Upstream == nil {
		return nil
	}
	return &upstream.Config{
		URL:         cfg.Upstream.URL,
		NodeID:      cfg.Upstream.NodeID,
		Token:       cfg.Upstream.Token,
		DisplayName: cfg.Upstream.DisplayName,
		Insecure:    cfg.Upstream.Insecure,
	}
}

// buildAgentOpts translates cfg.Agents into the session.AgentOpts map (router
// spawn path) and the cron.AgentOpts projection (toCronAgentOpts). Both maps
// are always non-nil.
func buildAgentOpts(cfg *config.Config) (map[string]session.AgentOpts, map[string]cron.AgentOpts) {
	agents := make(map[string]session.AgentOpts, len(cfg.Agents))
	for id, ac := range cfg.Agents {
		agents[id] = session.AgentOpts{
			Model:          ac.Model,
			ExtraArgs:      ac.Args,
			Effort:         ac.Effort,
			SystemPrompt:   ac.SystemPrompt,
			AccessProfile:  ac.AccessProfile,
			DefaultBackend: ac.Backend,
		}
	}
	cronAgents := make(map[string]cron.AgentOpts, len(agents))
	for id, a := range agents {
		cronAgents[id] = toCronAgentOpts(a)
	}
	return agents, cronAgents
}

// buildRouting is the routing the server and the upstream connector share: the
// agent maps and the one KeyResolver over them, which carries the cron
// access-profile lookup the server's remote-dispatch gate needs.
func buildRouting(cfg *config.Config, agents map[string]session.AgentOpts, projectMgr *project.Manager, sched *cron.Scheduler) server.RoutingOptions {
	return server.RoutingOptions{
		Agents:        agents,
		AgentCommands: cfg.AgentCommands,
		Resolver:      wireup.KeyResolver(agents, cfg.AgentCommands, projectMgr, sched),
	}
}

// logConfigValidationDiagnostics logs every config.Validate() finding at its
// level. Error-level diags do NOT abort startup: runtime skips unknown IDs
// gracefully (docs/rfc/multi-backend.md §11.1 fail-soft).
func logConfigValidationDiagnostics(cfg *config.Config) {
	for _, diag := range cfg.Validate() {
		switch diag.Level {
		case "error":
			slog.Error("config validation",
				"field", diag.Field, "msg", diag.Msg, "hint", diag.Hint)
		default:
			slog.Warn("config validation",
				"field", diag.Field, "msg", diag.Msg, "hint", diag.Hint)
		}
	}
}

// backendWrappers holds the result of initBackendWrappers.
type backendWrappers struct {
	// Runtimes is one row per backend — wrapper plus the per-backend config the
	// router needs — instead of the five parallel map[backendID]→property tables
	// this used to carry (G2 #2666). Effort is populated only for backends whose
	// Protocol accepts one; others are warned and dropped. ConfiguredModels is
	// the operator-declared manifest (cli.backends[].models); agent-reported
	// manifests win at request time.
	Runtimes map[string]session.BackendRuntime
	Default  *cli.Wrapper
	// DefaultID is Default.BackendID, the router's backend for sessions that
	// name none. It differs from cfg.DefaultBackendID when that id is unlisted
	// or unregistered; Validate warns about that fallback.
	DefaultID string
}

// wrapperMap projects Runtimes back to id→wrapper for the health-sibling check,
// which is about wrappers specifically.
func (b backendWrappers) wrapperMap() map[string]*cli.Wrapper {
	out := make(map[string]*cli.Wrapper, len(b.Runtimes))
	for id, rt := range b.Runtimes {
		if rt.Wrapper != nil {
			out[id] = rt.Wrapper
		}
	}
	return out
}

// initBackendWrappers constructs cli.Wrapper instances for every enabled
// backend and selects the default. Returns ok=false when no usable backend is
// configured or the default's --version probe failed with no healthy sibling;
// the caller emits the operator-facing slog.Error.
func initBackendWrappers(
	ctx context.Context,
	cfg *config.Config,
	shimMgr *shim.Manager,
) (backendWrappers, bool) {
	backendsCfg := cfg.EnabledBackends()
	defaultBackend := cfg.DefaultBackendID()

	out := backendWrappers{
		Runtimes: make(map[string]session.BackendRuntime, len(backendsCfg)),
	}

	for _, b := range backendsCfg {
		profile, ok := backend.Get(b.ID)
		if !ok {
			// Empty ID is a single-backend config; treat it as claude.
			if b.ID == "" {
				profile, ok = backend.Get("claude")
			}
			if !ok {
				slog.Warn("skipping unknown cli.backends entry", "id", b.ID)
				continue
			}
		}
		proto := profile.NewProtocol(backend.ProtocolDeps{})
		// NewWrapperLazy + Probe(ctx) so a hung `<cli> --version` cannot pin
		// startup for the full 5s when SIGTERM arrives mid-init.
		w := cli.NewWrapperLazy(b.Path, proto, b.ID).WithManager(shimMgr)
		w.Probe(ctx)
		rt := session.BackendRuntime{Wrapper: w}
		if b.Model != "" {
			rt.Model = b.Model
		}
		if len(b.Args) > 0 {
			rt.ExtraArgs = b.Args
		}
		if len(b.Models) > 0 {
			rt.ConfiguredModels = b.Models
		}
		// Capability check lives here (where the Protocol is built), not in
		// config validation. Warn rather than refuse to start: EnabledBackends()
		// propagates cli.effort to EVERY backend, so a mixed deployment setting
		// the top-level default would otherwise be unbootable.
		if b.Effort != "" {
			if cli.ProtocolCaps(proto).EffortTier {
				rt.Effort = b.Effort
			} else {
				cli.EmitSpawnDiags("config", []cli.SpawnDiag{{
					Layer:  "caps",
					Key:    "cli.backends[" + w.BackendID + "].effort",
					Action: "ignored",
					Reason: "backend does not accept a thinking-effort tier " +
						"(claude and ACP-protocol backends take --effort; codex's knob is " +
						"-c model_reasoning_effort= via that backend's args)",
				}})
			}
		}
		if out.Default == nil || w.BackendID == defaultBackend {
			out.Default = w
		}
		// Empty CLIVersion means `--version` failed. The wrapper stays
		// registered so the dashboard shows the intent, but spawns will fail;
		// Warn so operators notice at startup, not at the first message.
		if w.CLIVersion == "" {
			slog.Warn("cli backend version probe failed",
				"id", w.BackendID, "name", w.CLIName, "path", w.CLIPath,
				"hint", "binary missing or --version crashed; spawns will fail until resolved")
		} else {
			slog.Info("cli backend enabled",
				"id", w.BackendID, "name", w.CLIName,
				"path", w.CLIPath, "version", w.CLIVersion)
		}
		// Store the completed row LAST: the effort capability check above may
		// still be adding to it, and BackendRuntime is a value.
		out.Runtimes[w.BackendID] = rt
	}

	if out.Default == nil {
		return out, false
	}
	out.DefaultID = out.Default.BackendID
	// Default probe failed but a sibling is healthy: continue with a Warn so
	// explicit-backend sessions (e.g. sysession) stay usable; fast-fail only
	// when EVERY backend is unreachable (#903).
	if out.Default.CLIVersion == "" {
		if !backendsHaveHealthySibling(out.wrapperMap(), out.DefaultID) {
			return out, false
		}
		slog.Warn("default cli backend probe failed; healthy sibling(s) available — continuing startup",
			"default_id", out.DefaultID, "default_path", out.Default.CLIPath,
			"hint", "default-bound spawns will error until resolved; explicit-backend sessions remain usable")
	}
	_ = ctx
	return out, true
}

// backendsHaveHealthySibling reports whether any wrapper other than the
// default has a populated CLIVersion (#903).
func backendsHaveHealthySibling(wrappers map[string]*cli.Wrapper, defaultID string) bool {
	for id, w := range wrappers {
		if id == defaultID {
			continue
		}
		if w != nil && w.CLIVersion != "" {
			return true
		}
	}
	return false
}
