package main

import (
	"context"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/shim"
	"github.com/naozhi/naozhi/internal/upstream"
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
func newLogHandler(w *os.File, cfg *config.Config) slog.Handler {
	opts := &slog.HandlerOptions{Level: resolveLogLevel(cfg.Log.Level)}
	if cfg.Log.Format == "text" {
		return slog.NewTextHandler(w, opts)
	}
	return slog.NewJSONHandler(w, opts)
}

// setupLogging installs the process-global slog default logger from cfg,
// writing to stdout.
func setupLogging(cfg *config.Config) {
	slog.SetDefault(slog.New(newLogHandler(os.Stdout, cfg)))
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
			Model:        ac.Model,
			ExtraArgs:    ac.Args,
			Effort:       ac.Effort,
			SystemPrompt: ac.SystemPrompt,
		}
	}
	cronAgents := make(map[string]cron.AgentOpts, len(agents))
	for id, a := range agents {
		cronAgents[id] = toCronAgentOpts(a)
	}
	return agents, cronAgents
}

// firstUndefinedAgentCommand reports the first agent_commands entry whose
// target agent id is not in agents; ok=true means every command resolves.
// Keys are sorted so the reported command is deterministic across runs.
func firstUndefinedAgentCommand(agentCommands map[string]string, agents map[string]session.AgentOpts) (string, bool) {
	cmds := make([]string, 0, len(agentCommands))
	for cmd := range agentCommands {
		cmds = append(cmds, cmd)
	}
	sort.Strings(cmds)
	for _, cmd := range cmds {
		if _, ok := agents[agentCommands[cmd]]; !ok {
			return cmd, false
		}
	}
	return "", true
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
	Wrappers  map[string]*cli.Wrapper
	Models    map[string]string
	ExtraArgs map[string][]string
	// Efforts holds the per-backend thinking-effort tier, populated only for
	// backends whose Protocol accepts one; others are warned and dropped.
	Efforts map[string]string
	// ModelLists holds operator-declared per-backend model manifests
	// (cli.backends[].models); agent-reported manifests win at request time.
	ModelLists map[string][]string
	Default    *cli.Wrapper
	DefaultID  string
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
		Wrappers:   make(map[string]*cli.Wrapper, len(backendsCfg)),
		Models:     make(map[string]string, len(backendsCfg)),
		ExtraArgs:  make(map[string][]string, len(backendsCfg)),
		Efforts:    make(map[string]string, len(backendsCfg)),
		ModelLists: make(map[string][]string, len(backendsCfg)),
		DefaultID:  defaultBackend,
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
		out.Wrappers[w.BackendID] = w
		if b.Model != "" {
			out.Models[w.BackendID] = b.Model
		}
		if len(b.Args) > 0 {
			out.ExtraArgs[w.BackendID] = b.Args
		}
		if len(b.Models) > 0 {
			out.ModelLists[w.BackendID] = b.Models
		}
		// Capability check lives here (where the Protocol is built), not in
		// config validation. Warn rather than refuse to start: EnabledBackends()
		// propagates cli.effort to EVERY backend, so a mixed deployment setting
		// the top-level default would otherwise be unbootable.
		if b.Effort != "" {
			if cli.ProtocolCaps(proto).EffortTier {
				out.Efforts[w.BackendID] = b.Effort
			} else {
				slog.Warn("ignoring configured thinking-effort tier: backend does not accept one",
					"id", w.BackendID, "effort", b.Effort,
					"hint", "claude and ACP-protocol backends (kiro) take --effort; "+
						"codex does not (its knob is -c model_reasoning_effort=, "+
						"set it via that backend's args) — move the setting under a "+
						"supporting backend to silence this")
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
	}

	if out.Default == nil {
		return out, false
	}
	// Default probe failed but a sibling is healthy: continue with a Warn so
	// explicit-backend sessions (e.g. sysession) stay usable; fast-fail only
	// when EVERY backend is unreachable (#903).
	if out.Default.CLIVersion == "" {
		if !backendsHaveHealthySibling(out.Wrappers, out.DefaultID) {
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
