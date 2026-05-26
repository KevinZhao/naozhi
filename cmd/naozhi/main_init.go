package main

import (
	"context"
	"log/slog"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/shim"
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
	settingsFile string,
	refreshSettings func() string,
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
		proto := profile.NewProtocol(backend.ProtocolDeps{
			SettingsFile:    settingsFile,
			RefreshSettings: refreshSettings,
		})
		w := cli.NewWrapper(b.Path, proto, b.ID)
		w.ShimManager = shimMgr
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
