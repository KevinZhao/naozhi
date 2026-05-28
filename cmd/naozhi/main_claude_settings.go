// File: main_claude_settings.go
//
// Phase 5-prep / R-cmd-claude-settings-extract (2026-05-28):
// 把 main.go 中"Claude settings 处理 + naozhi callback hook 滤镜"区段抽
// 到独立文件。**纯物理切分，逐字保留原代码、零行为变化**。
//
// 抽出的内容（全部按 main.go line 53-452 原貌）：
//   - claudeEnvAllowedPrefixes / awsEnvDenyList 白/黑名单
//   - settingsErrSeverity enum + claudeSettingsErrSeverity 分类器
//   - readClaudeSettingsRaw / readJSONWithRetry 文件读取（带重试）
//   - filterClaudeEnv / matchesAnyPrefix 环境变量过滤
//   - applyClaudeEnvSettings 父进程 env 注入（保留 shell 优先权）
//   - writeClaudeSettingsOverride 生成 ~/.naozhi/claude-settings.json
//   - addrPort / filterHooks / sanitizeLogCmd / loopbackV4Re /
//     isNaozhiCallbackHook hook 回调滤镜
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
	"regexp"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
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
// Used by both applyClaudeEnvSettings (parent-process env injection) and
// writeClaudeSettingsOverride (child --settings file emission) so the two paths
// can never disagree on which keys are propagated. Historically AWS_PROFILE
// leaked into the override file (bypassing the parent deny list) and overrode
// the proxy-based bedrock auth chain; routing both paths through this helper
// closes that gap.
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

// writeClaudeSettingsOverride generates ~/.naozhi/claude-settings.json by copying
// ~/.claude/settings.json verbatim, but filtering out only the hook entries that
// would call back into naozhi (causing infinite loops). Safe hooks such as
// formatters and linters are preserved as-is.
//
// Returns the file path on success. On transient read/parse failures (common when
// Claude CLI is concurrently rewriting settings.json), RETAINS any existing
// ~/.naozhi/claude-settings.json from a prior successful run instead of overwriting
// it with `{}` — that empty file would strip the `env` block and break Bedrock auth
// for every spawned CLI process (the whole reason for --setting-sources "").
func writeClaudeSettingsOverride(ctx context.Context, serverAddr string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".naozhi")
	path := filepath.Join(dir, "claude-settings.json")

	data, err := readClaudeSettingsRaw(ctx)
	if err != nil {
		// Read/parse failed. Do NOT overwrite an existing override — the last
		// known-good copy still lets Claude CLI authenticate. Report via logs so
		// the operator notices the degraded mode.
		//
		// R236-QA-13: file-missing is the normal first-run state and stays at
		// Warn; corrupt-JSON / unreadable means somebody actively broke the
		// settings file and gets logged at Error so it surfaces in alerting.
		if errors.Is(err, fs.ErrNotExist) {
			slog.Warn("read ~/.claude/settings.json: file missing; keeping previous override", "err", err)
		} else {
			slog.Error("read ~/.claude/settings.json: corrupt or unreadable; keeping previous override", "err", err)
		}
		if _, statErr := os.Stat(path); statErr == nil {
			return path
		}
		// No prior override exists either. Fall through to writing an empty
		// object so --settings has *something* to point at; Claude will then
		// complain loudly ("Not logged in") and the log warn above is the clue.
		data = []byte("{}")
	}

	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil {
		// data came from readClaudeSettingsRaw which validates JSON, so this
		// path only fires on the {} fallback or a non-object top level.
		settings = make(map[string]json.RawMessage)
	}
	if settings == nil {
		settings = make(map[string]json.RawMessage)
	}

	port := addrPort(serverAddr)
	if hooksRaw, ok := settings["hooks"]; ok {
		settings["hooks"] = filterHooks(hooksRaw, port)
	}

	// Filter env section through the same allowlist+deny+value-safety check
	// applied to the parent process. Without this, keys like AWS_PROFILE that
	// applyClaudeEnvSettings refuses can still leak into spawned claude via the
	// override file, silently overriding the proxy-based bedrock auth chain.
	if envRaw, ok := settings["env"]; ok {
		var envMap map[string]string
		if err := json.Unmarshal(envRaw, &envMap); err == nil {
			filtered := filterClaudeEnv(envMap)
			if filteredRaw, err := json.Marshal(filtered); err == nil {
				settings["env"] = filteredRaw
			}
		}
	}

	out, err := json.Marshal(settings)
	if err != nil {
		return ""
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return ""
	}
	// Atomic write: claude reads this file on startup; a truncated write
	// could cause it to launch with empty config and disable hook filtering,
	// risking feedback loops.
	if err := osutil.WriteFileAtomic(path, out, 0600); err != nil {
		return ""
	}
	return path
}

// addrPort extracts the port number string from a listen address like ":8180" or "0.0.0.0:8180".
func addrPort(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i+1:]
	}
	return addr
}

// filterHooks returns hooksRaw with any individual hook entries that would call back
// into naozhi removed. It works at the entry level: a group loses only its dangerous
// entries; if all entries in a group are removed the whole group is dropped.
// If parsing fails, returns an empty hooks object to be safe.
func filterHooks(hooksRaw json.RawMessage, serverPort string) json.RawMessage {
	// hooks shape: map[eventName] → []{ "matcher":..., "hooks": []{ "type":..., "command":... } }
	var byEvent map[string][]map[string]json.RawMessage
	if err := json.Unmarshal(hooksRaw, &byEvent); err != nil {
		empty, _ := json.Marshal(map[string]any{})
		return empty
	}

	changed := false
	for eventName, groups := range byEvent {
		var keptGroups []map[string]json.RawMessage
		for _, group := range groups {
			entriesRaw, ok := group["hooks"]
			if !ok {
				keptGroups = append(keptGroups, group)
				continue
			}
			var entries []map[string]json.RawMessage
			if err := json.Unmarshal(entriesRaw, &entries); err != nil {
				keptGroups = append(keptGroups, group)
				continue
			}
			var safeEntries []map[string]json.RawMessage
			for _, e := range entries {
				var cmd string
				if raw, ok := e["command"]; ok {
					_ = json.Unmarshal(raw, &cmd)
				}
				if isNaozhiCallbackHook(cmd, serverPort) {
					changed = true
					slog.Info("dropping hook to prevent naozhi callback loop", "event", eventName, "command", sanitizeLogCmd(cmd))
				} else {
					safeEntries = append(safeEntries, e)
				}
			}
			if len(safeEntries) == 0 {
				changed = true
				continue // drop group entirely
			}
			if len(safeEntries) != len(entries) {
				changed = true
				newRaw, err := json.Marshal(safeEntries)
				if err != nil {
					continue // skip corrupted group
				}
				group["hooks"] = newRaw
			}
			keptGroups = append(keptGroups, group)
		}
		byEvent[eventName] = keptGroups
	}

	if !changed {
		return hooksRaw
	}
	out, _ := json.Marshal(byEvent)
	return out
}

// sanitizeLogCmd scrubs control characters from a hook command string so
// attacker-controlled content in ~/.claude/settings.json cannot inject fake
// log lines (newlines, ANSI escapes) when log.format is text. Also truncates
// to 80 chars so the log line stays readable.
func sanitizeLogCmd(cmd string) string {
	if len(cmd) > 80 {
		cmd = cmd[:80] + "..."
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '.'
		}
		return r
	}, cmd)
}

// loopbackV4Re matches a 127/8 IPv4 dotted-quad followed by ":<port>" so a
// substring like "version 127." or a hostname containing "foo127.example.com"
// does not produce a false positive. The leading boundary requires a non-
// digit / non-dot prefix (or the start of the string) so the digits cannot
// be a tail of some other number. R236-QA-20 (#544).
var loopbackV4Re = regexp.MustCompile(`(^|[^0-9a-z.])127\.\d{1,3}\.\d{1,3}\.\d{1,3}:`)

// isNaozhiCallbackHook reports whether a hook command appears to call back into
// naozhi's HTTP server (which would cause an infinite loop).
// It matches: any mention of "naozhi", or an HTTP call to localhost/127.0.0.1 on
// naozhi's listen port.
func isNaozhiCallbackHook(cmd, port string) bool {
	if cmd == "" {
		return false
	}
	lower := strings.ToLower(cmd)
	if strings.Contains(lower, "naozhi") {
		return true
	}
	if port != "" {
		for _, host := range []string{"localhost", "127.0.0.1", "0.0.0.0", "[::1]", "::1"} {
			if strings.Contains(lower, host+":"+port) {
				return true
			}
		}
		// Match any 127.x.x.x:port address (entire 127/8 loopback block).
		// R236-QA-20 (#544): the historical substring check
		// `strings.Contains(lower, "127.")` produced false positives on any
		// command containing the literal "127." even in unrelated contexts
		// (e.g. "version 127." or hostnames such as "foo127.example.com")
		// provided the same command also mentioned ":<port>" somewhere.
		// The regex requires a real dotted-quad shape next to the port
		// boundary so legitimate hooks survive while real loopback URLs
		// keep firing.
		if loopbackV4Re.MatchString(lower) && strings.Contains(lower, ":"+port) {
			return true
		}
	}
	return false
}
