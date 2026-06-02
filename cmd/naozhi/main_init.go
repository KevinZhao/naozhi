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
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/shim"
	"github.com/naozhi/naozhi/internal/upstream"
)

// File: main_init.go
//
// CQ1 (Round 174/194 → issue #396): main()'s 600+ line body grew past the
// readable budget. This file extracts pure-init helpers from main() that
// have no dependency on local context state beyond their inputs, so each
// helper can be unit-tested in isolation against fake config / fake shim
// manager. The helpers are intentionally small and side-effect-explicit
// (returns wrappers / logs / etc.) — bigger sub-systems (router init,
// platform init, scheduler init) keep their existing entry points and
// will be lifted in follow-up rounds.

// resolveLogLevel maps a config.Log.Level string to a slog.Level. Unknown
// or empty values fall back to Info. Extracted from main() (R237-ARCH-8 /
// #590) so the level mapping is unit-testable in isolation.
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

// newLogHandler builds the slog.Handler for the configured log format and
// level, writing to w. "text" selects a TextHandler; everything else (incl.
// the default "json") selects a JSONHandler. Extracted from main() so the
// handler-selection branch is testable without touching the process-global
// default logger (R237-ARCH-8 / #590).
func newLogHandler(w *os.File, cfg *config.Config) slog.Handler {
	opts := &slog.HandlerOptions{Level: resolveLogLevel(cfg.Log.Level)}
	if cfg.Log.Format == "text" {
		return slog.NewTextHandler(w, opts)
	}
	return slog.NewJSONHandler(w, opts)
}

// setupLogging installs the process-global slog default logger from cfg,
// writing to stdout. Thin wrapper over newLogHandler kept in main_init so
// main() stays a single SetDefault call (R237-ARCH-8 / #590).
func setupLogging(cfg *config.Config) {
	slog.SetDefault(slog.New(newLogHandler(os.Stdout, cfg)))
}

// startWatchdogLoop launches the systemd liveness heartbeat goroutine.
// WATCHDOG=1 is sent unconditionally every 30s (its purpose is OS-level
// liveness); the router HealthCheck result is logged as a diagnostic only
// and never suppresses the heartbeat — normal write-lock activity (cleanup,
// spawn) would otherwise cause false negatives. Returns when ctx is done.
// Extracted from main() (R237-ARCH-8 / #590) so the heartbeat cadence and
// HealthCheck-does-not-gate contract are exercisable in isolation.
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
// cfg.Nodes. Returns nil when no nodes are configured (the server treats a
// nil and an empty map identically). Extracted from main() (R237-ARCH-8 /
// #590) so the per-node HTTP client construction is testable without the
// rest of startup; the slog "multi-node configured" line stays in main()
// to keep startup-log output byte-stable.
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

// buildReverseNodeAuth translates cfg.ReverseNodes (config.ReverseNodeEntry)
// into the node-package's zero-dependency node.ReverseNodeAuth shape, so
// internal/node no longer imports internal/config (R040034-ARCH-1 / #1411).
// The translation lives at the cmd boundary — the only place that already
// depends on both packages. Returns nil when no reverse nodes are configured
// so the caller's len()>0 guard stays meaningful.
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

// buildUpstreamConfig translates config.UpstreamConfig into the upstream
// package's zero-dependency upstream.Config value, so internal/upstream no
// longer imports internal/config (R040034-ARCH-1 / #1411). Returns nil when
// cfg.Upstream is nil so the caller's nil guard stays meaningful.
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

// buildAgentOpts translates cfg.Agents into the two views main() needs: the
// session.AgentOpts map (operator-trusted shape consumed by the router spawn
// path) and the cron.AgentOpts map (the internal/cron-import-free projection
// produced by toCronAgentOpts). Extracted from main() (R237-ARCH-8 / #590)
// so the model/args copy and the cron translation are unit-testable without
// booting the router. Both maps are always non-nil (possibly empty).
func buildAgentOpts(cfg *config.Config) (map[string]session.AgentOpts, map[string]cron.AgentOpts) {
	agents := make(map[string]session.AgentOpts, len(cfg.Agents))
	for id, ac := range cfg.Agents {
		agents[id] = session.AgentOpts{
			Model:     ac.Model,
			ExtraArgs: ac.Args,
		}
	}
	cronAgents := make(map[string]cron.AgentOpts, len(agents))
	for id, a := range agents {
		cronAgents[id] = toCronAgentOpts(a)
	}
	return agents, cronAgents
}

// firstUndefinedAgentCommand reports the first agent_commands entry whose
// target agent id is not present in agents. ok=true means every command
// resolves (cmd is then ""); ok=false surfaces the offending command so
// main() can emit the operator-actionable os.Exit log unchanged. Extracted
// from main() (R237-ARCH-8 / #590) so the cross-reference validation is
// testable independent of process exit. Iteration order over a Go map is
// unspecified, but the contract only promises "a" failing command, matching
// the original loop's fail-on-first-seen behavior.
func firstUndefinedAgentCommand(agentCommands map[string]string, agents map[string]session.AgentOpts) (string, bool) {
	for cmd, agentID := range agentCommands {
		if _, ok := agents[agentID]; !ok {
			return cmd, false
		}
	}
	return "", true
}

// logConfigValidationDiagnostics surfaces every config.Validate() finding
// to the structured log at the appropriate level. docs/rfc/multi-backend.md
// §11.1: warn-and-continue posture — error-level diags do NOT abort startup
// because runtime gracefully skips unknown IDs and erroring here would
// defeat the multi-backend rollout's "fail-soft" design.
//
// Extracted from main() so a config regression that floods the log with
// validation warnings is unit-testable via a fake cfg.Validate() return.
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

// backendWrappers holds the result of initBackendWrappers — flattened so
// the caller can range over fields rather than a positional return list,
// and so future additions (e.g. per-backend metrics handles) extend the
// struct without changing every call site.
type backendWrappers struct {
	Wrappers  map[string]*cli.Wrapper
	Models    map[string]string
	ExtraArgs map[string][]string
	Default   *cli.Wrapper
	DefaultID string
}

// initBackendWrappers constructs cli.Wrapper instances for every enabled
// backend in cfg.EnabledBackends() and selects the default. Returns the
// initialised set together with the per-backend model / args overrides that
// session.Router uses to spawn the right binary for a given session key.
//
// Extracted from main() (CQ1 / issue #396). Pure aside from cli.NewWrapper's
// `--version` probe (which the existing wrapper already isolates), so a
// fake backend.Profile + fake config makes this helper unit-testable.
//
// Returns ok=false when no usable backend is configured OR the default
// backend's --version probe failed; the caller is responsible for emitting
// the corresponding slog.Error with operator-actionable hints (kept in
// main() so the current journalctl format is byte-stable across the
// refactor).
func initBackendWrappers(
	ctx context.Context,
	cfg *config.Config,
	shimMgr *shim.Manager,
) (backendWrappers, bool) {
	backendsCfg := cfg.EnabledBackends()
	defaultBackend := cfg.DefaultBackendID()

	out := backendWrappers{
		Wrappers:  make(map[string]*cli.Wrapper, len(backendsCfg)),
		Models:    make(map[string]string, len(backendsCfg)),
		ExtraArgs: make(map[string][]string, len(backendsCfg)),
		DefaultID: defaultBackend,
	}

	for _, b := range backendsCfg {
		profile, ok := backend.Get(b.ID)
		if !ok {
			// Empty ID is a legacy single-backend config; treat it as claude
			// to preserve the historical default.
			if b.ID == "" {
				profile, ok = backend.Get("claude")
			}
			if !ok {
				slog.Warn("skipping unknown cli.backends entry", "id", b.ID)
				continue
			}
		}
		proto := profile.NewProtocol(backend.ProtocolDeps{})
		// DEADCODE-6 / R241-ARCH-1: use NewWrapperLazy + Probe(ctx) so a hung
		// `<cli> --version` cannot pin startup for the full 5 s when SIGTERM
		// arrives mid-init. NewWrapper is the legacy synchronous form (still
		// kept for tests that don't have a stopCtx). Production startup gets
		// the cancellable variant — same field shape, so downstream readers of
		// w.CLIVersion (the "backend X version Y" banner below at line 123-124)
		// see identical values.
		w := cli.NewWrapperLazy(b.Path, proto, b.ID).WithManager(shimMgr)
		w.Probe(ctx)
		out.Wrappers[w.BackendID] = w
		if b.Model != "" {
			out.Models[w.BackendID] = b.Model
		}
		if len(b.Args) > 0 {
			out.ExtraArgs[w.BackendID] = b.Args
		}
		if out.Default == nil || w.BackendID == defaultBackend {
			out.Default = w
		}
		// Empty CLIVersion means `--version` failed (binary missing, wrong
		// path, or crash). The wrapper is still registered so the dashboard
		// surfaces the configuration intent, but spawn attempts will fail.
		// Log at Warn so operators notice during startup instead of
		// discovering the breakage only when the first user message lands.
		// R55-QUAL-001.
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
	// R245-ARCH-43 (#903): when the default backend's --version probe failed
	// but at least one sibling backend is healthy, continue startup with a
	// Warn rather than os.Exit(1). The original R55-QUAL-001 contract was
	// "fail fast on default-probe failure so operators don't see every IM
	// message bounce" — but on heterogeneous deployments where sysession (or
	// any explicit-backend session) targets a non-default backend, that exit
	// makes the healthy fallback path unreachable. Default-bound requests
	// still error at spawn time with the existing "spawn failed" message,
	// which is the correct surface for routing-not-configured errors; the
	// startup-time fast-fail is preserved only when EVERY backend is
	// unreachable.
	if out.Default.CLIVersion == "" {
		if !backendsHaveHealthySibling(out.Wrappers, out.DefaultID) {
			return out, false
		}
		slog.Warn("default cli backend probe failed; healthy sibling(s) available — continuing startup",
			"default_id", out.DefaultID, "default_path", out.Default.CLIPath,
			"hint", "default-bound spawns will error until resolved; explicit-backend sessions remain usable")
	}
	// ctx is reserved for a future cancellable probe path (RFC §11.4) and
	// kept on the signature now so callers don't need to update later.
	_ = ctx
	return out, true
}

// backendsHaveHealthySibling reports whether any wrapper other than the
// default has a populated CLIVersion. R245-ARCH-43 (#903): this is the
// "fallback path is reachable" check — when true, initBackendWrappers
// returns ok=true with a Warn instead of forcing os.Exit(1) on a default-
// probe failure, so heterogeneous deployments (sysession or explicit-
// backend sessions on a healthy alt) can still operate.
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
