// Parent-process env injection from ~/.claude/settings.json
// (docs/rfc/direct-user-settings.md §7.1). cc reads settings.json itself via
// `--setting-sources user`; this path only feeds naozhi (transcribe → Bedrock)
// and the sysession Runner.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/envpolicy"
	"github.com/naozhi/naozhi/internal/naozhisettings"
	"github.com/naozhi/naozhi/internal/osutil"
)

// settingsErrSeverity classifies applyClaudeEnvSettings failures for main():
// file-missing (first run) and ctx-cancel (shutdown) stay at Warn; corrupt
// JSON is operator-actionable and surfaces at Error.
type settingsErrSeverity int

const (
	settingsErrSeverityFatal settingsErrSeverity = iota
	settingsErrSeverityCancel
	settingsErrSeverityMissing
)

func claudeSettingsErrSeverity(err error) settingsErrSeverity {
	switch {
	case err == nil:
		return settingsErrSeverityFatal // unreachable; caller already nil-checked
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return settingsErrSeverityCancel
	case errors.Is(err, fs.ErrNotExist):
		return settingsErrSeverityMissing
	default:
		return settingsErrSeverityFatal
	}
}

// readClaudeSettingsRaw reads ~/.claude/settings.json, retrying on invalid
// JSON because another process may be rewriting the file non-atomically.
// An error means "no trustworthy snapshot", NOT "file is empty".
func readClaudeSettingsRaw(ctx context.Context) ([]byte, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("user home: %w", err)
	}
	path := filepath.Join(home, ".claude", "settings.json")
	return readJSONWithRetry(ctx, path, 3, 100*time.Millisecond)
}

// readJSONWithRetry reads path and retries up to attempts-1 times if the
// content is not valid JSON. A read error returns immediately (missing is a
// different failure mode than truncated); ctx cancellation aborts a sleep.
func readJSONWithRetry(ctx context.Context, path string, attempts int, sleep time.Duration) ([]byte, error) {
	return readJSONWithRetryFn(ctx, path, attempts, sleep, os.ReadFile)
}

// readJSONWithRetryFn is readJSONWithRetry with the file reader injected so
// tests can pin the torn-read interleaving deterministically (#2473).
func readJSONWithRetryFn(ctx context.Context, path string, attempts int, sleep time.Duration, readFile func(string) ([]byte, error)) ([]byte, error) {
	var lastParseErr error
	for i := 0; i < attempts; i++ {
		data, err := readFile(path)
		if err != nil {
			return nil, err
		}
		if json.Valid(data) {
			return data, nil
		}
		lastParseErr = fmt.Errorf("invalid JSON (attempt %d/%d, %d bytes)", i+1, attempts, len(data))
		if i < attempts-1 {
			t := time.NewTimer(sleep)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			}
		}
	}
	return nil, lastParseErr
}

// filterClaudeEnv returns the entries of in whose key the envpolicy Table
// allows for SourceSettings (Claude/AWS/proxy plumbing minus the
// auth-source-changing and kill-switch keys) and whose value passes both the
// generic checks (no NUL/newline, ≤4096 bytes) and the key's per-source guard
// (https-for-non-loopback for base URLs and proxies). Rejected keys are
// logged at WARN; keys outside the allowed namespaces are skipped silently.
// cc children do not go through this path (they read settings.json directly),
// so the parent-env view may intentionally differ from cc's (RFC §7.1).
func filterClaudeEnv(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		rule, allowed := envpolicy.Allowed(k, envpolicy.SourceSettings)
		if !allowed {
			// An explicitly denied key (matched a table rule) is worth a
			// warning — the operator put it in settings.json expecting effect;
			// a key outside the allowed namespaces is everyday noise.
			if rule.Pattern != "" {
				if strings.HasPrefix(k, "AWS_") {
					slog.Warn("claude settings env: refusing to propagate auth-source AWS var", "key", k)
				} else {
					slog.Warn("claude settings env: refusing to propagate CLAUDE_ kill-switch var", "key", k)
				}
			}
			continue
		}
		// Children inherit the env via execve; NUL/newline or huge values must
		// not reach it.
		if strings.ContainsAny(v, "\x00\n\r") || len(v) > 4096 {
			slog.Warn("claude settings env: rejecting unsafe value", "key", k, "len", len(v))
			continue
		}
		// Base URLs and proxies steer API / outbound traffic; a tampered file
		// could aim them at an attacker host or IMDS. https unless loopback
		// (#1576, #1660).
		if guard := envpolicy.GuardFor(k, envpolicy.SourceSettings); guard != nil {
			if err := guard(v); err != nil {
				slog.Warn("claude settings env: rejecting unsafe base_url", "key", k, "err", err)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// applyClaudeEnvSettings applies the settings.json env section to the current
// process for keys passing filterClaudeEnv; shell-set vars take precedence.
// Zero env applied is not an error; an unreadable/unparsable file is.
func applyClaudeEnvSettings(ctx context.Context) error {
	data, err := readClaudeSettingsRaw(ctx)
	if err != nil {
		return err
	}
	var s struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("unmarshal env section: %w", err)
	}
	for k, v := range filterClaudeEnv(s.Env) {
		if _, exists := os.LookupEnv(k); !exists {
			if err := os.Setenv(k, v); err != nil {
				slog.Warn("claude settings env: setenv failed", "key", k, "err", err)
			}
		}
	}
	return nil
}

// resolveNaozhiSettingsFile returns the absolute path of the opt-in
// naozhi-owned isolated Claude settings file for SpawnOptions.SettingsFile,
// or "" to keep `--setting-sources user` (RFC naozhi-owned-settings-v3). When
// enabled the file is bootstrapped ONCE from the local settings with
// hooks+env stripped; a bootstrap warning is non-fatal as long as a file exists.
func resolveNaozhiSettingsFile(cfg *config.Config, storePath, claudeDir string) string {
	if !cfg.NaozhiSettings.Enabled {
		return ""
	}
	path := osutil.ExpandHome(cfg.NaozhiSettings.Path)
	if path == "" {
		// Default next to the session store; CWD only when storePath is unset.
		base := "."
		if storePath != "" {
			base = filepath.Dir(storePath)
		}
		path = filepath.Join(base, "naozhi-settings.json")
	}
	// MUST be absolute: BuildArgs silently falls back to `--setting-sources
	// user` for a relative --settings, re-reading the file the operator opted
	// OUT of. Refuse to enable rather than appear enabled while using local.
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			slog.Error("naozhi settings: cannot resolve absolute path; staying on local settings",
				"path", path, "err", err)
			return ""
		}
		path = abs
	}
	localPath := ""
	if claudeDir != "" {
		localPath = filepath.Join(claudeDir, "settings.json")
	}
	existed, seeded, err := naozhisettings.EnsureBootstrapped(path, localPath)
	if err != nil {
		// err may be advisory (file written) or fatal (no file). Never hand the
		// router a --settings path to a missing file — cc would read no settings.
		if _, statErr := os.Stat(path); statErr != nil {
			slog.Error("naozhi settings: could not create isolated file; staying on local settings",
				"path", path, "err", err)
			return ""
		}
		slog.Warn("naozhi settings: bootstrapped isolated file with a warning",
			"path", path, "seeded_from_local", seeded, "warn", err)
		return path
	}
	if existed {
		slog.Info("naozhi settings: using existing isolated settings file", "path", path)
	} else {
		slog.Info("naozhi settings: bootstrapped isolated settings file",
			"path", path, "seeded_from_local", seeded)
	}
	return path
}
