// File: main_claude_settings.go
//
// Claude settings handling for the naozhi parent process.
//
// docs/rfc/direct-user-settings.md PR1 (2026-05-30): naozhi-spawned cc now
// loads ~/.claude/settings.json directly via `--setting-sources user`, so the
// settings-override copy (writeClaudeSettingsOverride) and the naozhi-callback
// hook filter (filterHooks / isNaozhiCallbackHook / sanitizeLogCmd / addrPort /
// loopbackV4Re) were removed — hook feedback-loop protection lives at naozhi's
// HTTP entry auth instead.
//
// What remains here is the **parent-process env injection** path (RFC §7.1),
// which is independent of cc's --setting-sources and still required so naozhi
// itself (transcribe → Bedrock) and the sysession Runner inherit the
// settings.json env block:
//   - claudeEnvAllowedPrefixes / awsEnvDenyList 白/黑名单
//   - settingsErrSeverity enum + claudeSettingsErrSeverity 分类器
//   - readClaudeSettingsRaw / readJSONWithRetry 文件读取（带重试）
//   - filterClaudeEnv / matchesAnyPrefix 环境变量过滤
//   - applyClaudeEnvSettings 父进程 env 注入（保留 shell 优先权）
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
)

// claudeEnvAllowedPrefixes restricts which env vars from
// ~/.claude/settings.json are allowed to leak into the naozhi parent process.
// Historically every key was injected, which meant arbitrary keys set by a
// third-party Claude extension would become part of naozhi's attack surface
// (and downstream shim/CLI env) with no audit. Limit to the prefixes that
// Claude CLI and its AWS/Anthropic model plumbing actually consume.
var claudeEnvAllowedPrefixes = []string{
	"ANTHROPIC_",
	"CLAUDE_",
	"AWS_",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
}

// awsEnvDenyList 在 AWS_ 前缀允许之上再画一条"禁止跨入"的线：这些键会
// 改变 AWS 认证来源（角色切换、凭据文件重定向），~/.claude/settings.json
// 可以被 Claude tool 写入，放行等于给一个不受控的 env 注入通道对
// transcribe/S3 执行凭据劫持。默认只允许标准的 region/credentials/session
// 组合走进子进程。
var awsEnvDenyList = map[string]bool{
	"AWS_ROLE_ARN":                true,
	"AWS_WEB_IDENTITY_TOKEN_FILE": true,
	"AWS_SHARED_CREDENTIALS_FILE": true,
	"AWS_CONFIG_FILE":             true,
	"AWS_PROFILE":                 true,
	"AWS_DEFAULT_PROFILE":         true,
	"AWS_CA_BUNDLE":               true,
	"AWS_ENDPOINT_URL":            true,
}

// settingsErrSeverity classifies the outcome of applyClaudeEnvSettings so
// main() can route to slog.Warn vs slog.Error consistently and the
// classification itself is unit-testable. R236-QA-13 (#542): file-missing
// is a legitimate first-run state and stays at Warn; corrupt JSON is
// operator-actionable and surfaces at Error so the SLO log filter picks
// it up. R241-GO-4 (#490): ctx-cancel mid-retry stays at Warn so
// shutdown noise does not pollute the corruption alerting filter.
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

// readClaudeSettingsRaw reads ~/.claude/settings.json and returns its raw bytes,
// retrying a few times if JSON parsing fails. The retry handles the race where
// another process (Claude CLI, a VS Code extension, etc.) is rewriting the file
// non-atomically: we may observe a truncated view, but 100ms later the writer
// has finished and we see a complete document.
//
// Returns (data, nil) on success. Returns a non-nil error if the file cannot be
// read, or if every retry yielded invalid JSON — callers must treat the error
// as "could not determine a trustworthy settings snapshot", NOT as "file is empty".
func readClaudeSettingsRaw(ctx context.Context) ([]byte, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("user home: %w", err)
	}
	path := filepath.Join(home, ".claude", "settings.json")
	return readJSONWithRetry(ctx, path, 3, 100*time.Millisecond)
}

// readJSONWithRetry reads path and verifies the content is valid JSON. If the
// read succeeds but parsing fails, retries up to attempts-1 more times with the
// given sleep in between. If the file doesn't exist, returns the os.Open error
// immediately (no retry — missing is a different failure mode than truncated).
// The ctx parameter allows callers to abort a retry sleep early on shutdown or
// timeout; ctx.Err() is returned when the context is cancelled mid-sleep.
func readJSONWithRetry(ctx context.Context, path string, attempts int, sleep time.Duration) ([]byte, error) {
	var lastParseErr error
	for i := 0; i < attempts; i++ {
		data, err := os.ReadFile(path)
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

// filterClaudeEnv returns a copy of in containing only entries that pass the
// allowlist (claudeEnvAllowedPrefixes), the deny list (awsEnvDenyList), and the
// per-value safety check (no NUL/newline, ≤4096 bytes). Rejected keys are
// logged at WARN once per call so operators can spot a malicious or misconfigured
// ~/.claude/settings.json.
//
// Used by applyClaudeEnvSettings (parent-process env injection). The
// awsEnvDenyList guards naozhi's own AWS auth source (transcribe → Bedrock,
// sysession Runner) against an auth-source override smuggled in via the
// settings.json env block. cc child processes no longer go through this path:
// post direct-user-settings PR1 they read settings.json directly via
// `--setting-sources user`, so the parent-env view and the cc-env view of the
// same settings.json may differ (RFC §7.1, documented intentional asymmetry).
func filterClaudeEnv(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if !matchesAnyPrefix(k, claudeEnvAllowedPrefixes) {
			continue
		}
		if awsEnvDenyList[k] {
			slog.Warn("claude settings env: refusing to propagate auth-source AWS var", "key", k)
			continue
		}
		// R188-SEC-M1: the prefix allowlist restricts key namespace but puts
		// no constraint on the value. A malicious ~/.claude/settings.json
		// could set ANTHROPIC_BASE_URL to an attacker-controlled host or
		// inject NUL/newline into the process env that child processes
		// inherit via execve. Gate the value size + reject NUL/newline.
		if strings.ContainsAny(v, "\x00\n\r") || len(v) > 4096 {
			slog.Warn("claude settings env: rejecting unsafe value", "key", k, "len", len(v))
			continue
		}
		out[k] = v
	}
	return out
}

// applyClaudeEnvSettings reads ~/.claude/settings.json and applies any env section
// to the current process so spawned CC child processes inherit them via os.Environ().
// Only sets vars not already present (shell-set vars take precedence) and only
// for keys passing filterClaudeEnv.
//
// Returns an error when the settings file cannot be read or parsed so callers
// can surface the failure. A nil return with zero env applied (e.g. no `env`
// section or all keys filtered) is NOT treated as an error.
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

// matchesAnyPrefix reports whether s starts with any of the given prefixes.
// Prefixes ending in "_" match a namespace; prefixes without "_" match the
// full name (e.g. "HTTP_PROXY" matches only the exact env name).
func matchesAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasSuffix(p, "_") {
			if strings.HasPrefix(s, p) {
				return true
			}
		} else if s == p {
			return true
		}
	}
	return false
}
