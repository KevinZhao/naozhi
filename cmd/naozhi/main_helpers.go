// File: main_helpers.go
//
// Phase 5-prep / R-cmd-main-helpers-extract (2026-05-28):
// 把 main.go 后段的 lifecycle helpers 抽到独立文件。**纯物理切分，
// 逐字保留原代码、零行为变化**。
//
// 抽出的内容（按 origin/master main.go line 1036-1335 原貌）：
//   - initPlatforms              IM 平台 adapter 构造（CQ1 #396 testability）
//   - parseDurationOrDefault     字符串 → time.Duration（带默认）
//   - parseBytesOrDefault        "50MB" → int64（带默认）
//   - stateDirWarnMB const       ~/.naozhi 软上限
//   - warnIfStateDirLarge        启动期磁盘占用预警
//   - chatIDSuffix               日志-only chat ID 截断 helper
//   - logWebhookEndpoints        启动期 webhook URL 打印
//   - buildSysessionManager      sysession.Manager 构造（含 Runner / Daemons 配置）
//
// 与 main_init.go (CQ1 #396) / main_claude_settings.go 同款 main_*.go
// 切分模式：抽 main() 中 lifecycle 主线之外的 helper 出去。同包可见性
// 让 main() caller 零改动。
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"
	discordplatform "github.com/naozhi/naozhi/internal/platform/discord"
	"github.com/naozhi/naozhi/internal/platform/feishu"
	slackplatform "github.com/naozhi/naozhi/internal/platform/slack"
	weixinplatform "github.com/naozhi/naozhi/internal/platform/weixin"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sysession"
	"github.com/naozhi/naozhi/internal/transcribe"
)

// initPlatforms wires each configured IM platform adapter into a map.
// Extracted from main() for testability + readability (CQ1). Callers
// still own lifecycle — initPlatforms neither starts goroutines nor
// touches globals; it just constructs the adapters and returns them.
// The transcribe service is threaded through so Feishu can accept voice
// messages; other adapters do not need it today.
func initPlatforms(cfg *config.Config, stt transcribe.Service) (map[string]platform.Platform, error) {
	platforms := make(map[string]platform.Platform)
	if cfg.Platforms.Feishu != nil {
		f := feishu.New(feishu.Config{
			AppID:                cfg.Platforms.Feishu.AppID,
			AppSecret:            cfg.Platforms.Feishu.AppSecret,
			ConnectionMode:       cfg.Platforms.Feishu.ConnectionMode,
			VerificationToken:    cfg.Platforms.Feishu.VerificationToken,
			EncryptKey:           cfg.Platforms.Feishu.EncryptKey,
			MaxReplyLen:          cfg.Platforms.Feishu.MaxReplyLength,
			AllowInsecureWebhook: cfg.Platforms.Feishu.AllowInsecureWebhook,
		}, stt)
		platforms["feishu"] = f
	}
	if cfg.Platforms.Slack != nil {
		s := slackplatform.New(slackplatform.Config{
			BotToken:    cfg.Platforms.Slack.BotToken,
			AppToken:    cfg.Platforms.Slack.AppToken,
			MaxReplyLen: cfg.Platforms.Slack.MaxReplyLength,
		})
		platforms["slack"] = s
	}
	if cfg.Platforms.Discord != nil {
		d := discordplatform.New(discordplatform.Config{
			BotToken:    cfg.Platforms.Discord.BotToken,
			MaxReplyLen: cfg.Platforms.Discord.MaxReplyLength,
		})
		platforms["discord"] = d
	}
	if cfg.Platforms.Weixin != nil {
		wx := weixinplatform.New(weixinplatform.Config{
			Token:       cfg.Platforms.Weixin.Token,
			BaseURL:     cfg.Platforms.Weixin.BaseURL,
			MaxReplyLen: cfg.Platforms.Weixin.MaxReplyLength,
		})
		platforms["weixin"] = wx
	}
	return platforms, nil
}

// parseDurationOrDefault parses a duration string, returning def on empty or error.
func parseDurationOrDefault(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

// parseBytesOrDefault parses a human-readable byte size string (e.g. "50MB", "1GB").
// Returns def on empty or unrecognized format.
func parseBytesOrDefault(s string, def int64) int64 {
	if s == "" {
		return def
	}
	s = strings.TrimSpace(s)
	s = strings.ToUpper(s)

	multiplier := int64(1)
	switch {
	case strings.HasSuffix(s, "GB"):
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "KB"):
		multiplier = 1024
		s = strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}

	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return def
	}
	return n * multiplier
}

// stateDirWarnMB is the soft ceiling for ~/.naozhi/ total size; see
// docs/ops/disk-budget.md. RNEW-OPS-415 tracks quota enforcement.
const stateDirWarnMB = 500

// warnIfStateDirLarge walks stateDir once at startup and warns if total
// bytes exceed stateDirWarnMB. First-run / permission errors are silent;
// a truncated scan still warns using the partial total as a lower bound.
func warnIfStateDirLarge(stateDir string) {
	if stateDir == "" || stateDir == "." {
		return
	}
	bytes, err := osutil.StateDirSize(stateDir)
	truncated := errors.Is(err, osutil.ErrStateDirScanTruncated)
	if err != nil && !truncated {
		return
	}
	sizeMB := bytes / (1024 * 1024)
	if sizeMB < stateDirWarnMB {
		return
	}
	slog.Warn("state directory large",
		"path", stateDir, "size_mb", sizeMB, "threshold_mb", stateDirWarnMB,
		"truncated", truncated,
		"hint", "enable the attachment-gc daemon to reclaim old attachments; prune events; see docs/ops/disk-budget.md")
}

// chatIDSuffix returns the last 8 characters of a chat ID for logging,
// prefixed with "…" so a grep on full IDs does not match. Empty input
// returns an empty string. Kept local to this file since it is log-only
// and does not need to round-trip.
func chatIDSuffix(id string) string {
	if id == "" {
		return ""
	}
	if len(id) <= 8 {
		return id
	}
	return "…" + id[len(id)-8:]
}

// logWebhookEndpoints prints a one-line summary of the webhook URLs operators
// need to paste into the IM vendor console. Platforms that do not expose a
// webhook route (e.g. feishu websocket mode) are skipped.
func logWebhookEndpoints(cfg *config.Config, platforms map[string]platform.Platform) {
	addr := cfg.Server.Addr
	if strings.HasPrefix(addr, ":") {
		addr = "0.0.0.0" + addr
	}
	for name := range platforms {
		switch name {
		case "feishu":
			if cfg.Platforms.Feishu != nil && cfg.Platforms.Feishu.ConnectionMode == "webhook" {
				slog.Info("platform webhook endpoint", "platform", name, "path", "/webhook/feishu", "addr", addr)
			}
		case "slack":
			// slack events api + socket mode: route is only exposed when not using socket mode
			if cfg.Platforms.Slack != nil && cfg.Platforms.Slack.AppToken == "" {
				slog.Info("platform webhook endpoint", "platform", name, "path", "/webhook/slack", "addr", addr)
			}
		case "weixin":
			slog.Info("platform webhook endpoint", "platform", name, "path", "/webhook/weixin", "addr", addr)
		}
	}
}

// workspaceRootLister unions the attachment-gc daemon's workspace-root
// sources — router default + per-chat overrides, and bound project
// paths — then normalises (abs + EvalSymlinks) and dedupes so the same
// directory reached via two strings is swept once
// (docs/rfc/attachment-gc-daemon.md §4.4 E1/E2). Either source may be
// nil (e.g. projects disabled); the lister copes.
type workspaceRootLister struct {
	router     *session.Router
	projectMgr *project.Manager
}

// KnownWorkspaceRoots implements sysession.WorkspaceRootLister.
func (l workspaceRootLister) KnownWorkspaceRoots() []string {
	var raw []string
	if l.router != nil {
		raw = append(raw, l.router.WorkspaceRoots()...)
	}
	if l.projectMgr != nil {
		for _, p := range l.projectMgr.All() {
			if p != nil && p.Path != "" {
				raw = append(raw, p.Path)
			}
		}
	}
	// Normalise + dedupe. EvalSymlinks collapses symlink/.. aliases to a
	// canonical path; failures (dir absent) fall back to the abs form so
	// a not-yet-created root is still swept once it exists.
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		canon, err := filepath.Abs(p)
		if err != nil {
			canon = p
		}
		if resolved, err := filepath.EvalSymlinks(canon); err == nil {
			canon = resolved
		}
		if _, dup := seen[canon]; dup {
			continue
		}
		seen[canon] = struct{}{}
		out = append(out, canon)
	}
	return out
}

// buildSysessionManager wires sysession.Manager from cfg.Sysession.
//
// Returns (nil, nil) when the framework is disabled — that's the
// happy path for deployments that don't want background daemons yet.
//
// Returns (nil, err) when enabled but unusable (e.g. work dir cannot
// be chmodded 0700, default backend has no binary path).  Caller
// should log the error and proceed without daemons rather than
// aborting startup — sysession is opt-in infrastructure, not a
// release-critical path.
//
// Step 11 will replace the nil OnRunStarted/OnRunEnded with WS-hub
// callbacks; Phase 1 ships without them so the dashboard reads fall
// back to polling /api/system/daemons.
func buildSysessionManager(cfg *config.Config, router *session.Router,
	projectMgr *project.Manager, defaultWrapper *cli.Wrapper, storePath string,
) (*sysession.Manager, string, error) {
	if !cfg.Sysession.Enabled {
		// Return nil rather than a no-op Manager so the caller's nil
		// guard is meaningful — main.go's Start/Stop loops both check
		// `if sysMgr != nil`, and a stubbed always-non-nil result would
		// turn that into dead code.
		return nil, "", nil
	}

	// Resolve work dir: explicit override first, then a sibling of
	// sessions.json (= dataDir/sys-sessions/).  Empty storePath means
	// the operator opted out of state persistence; fall back to ~/.naozhi
	// to keep the directory under user control.
	workDir := osutil.ExpandHome(cfg.Sysession.Runner.WorkDir)
	if workDir == "" {
		base := filepath.Dir(storePath)
		if base == "" || base == "." {
			home, _ := os.UserHomeDir()
			base = filepath.Join(home, ".naozhi")
		}
		workDir = filepath.Join(base, "sys-sessions")
	}
	resolvedWorkDir, err := sysession.EnsureWorkDir(workDir)
	if err != nil {
		return nil, "", fmt.Errorf("ensure sys-sessions dir: %w", err)
	}

	// Startup sweep — non-fatal; a busted directory should not block
	// daemon startup.  Default 7 days when unset; "0" disables.
	jsonlMaxAge := 7 * 24 * time.Hour
	if v := cfg.Sysession.Runner.JSONLMaxAge; v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			slog.Warn("sysession: bad jsonl_max_age; using default 7d", "err", err, "value", v)
		} else {
			jsonlMaxAge = parsed
		}
	}
	if _, err := sysession.SweepOldJSONL(resolvedWorkDir, jsonlMaxAge); err != nil {
		slog.Warn("sysession: startup sweep failed", "err", err, "dir", resolvedWorkDir)
	}

	// Build Runner from the default backend's binary.
	binPath := ""
	if defaultWrapper != nil {
		binPath = defaultWrapper.CLIPath
	}
	runner, err := sysession.NewRunner(sysession.RunnerConfig{
		BinPath: binPath,
		WorkDir: resolvedWorkDir,
		Model:   cfg.Sysession.Runner.Model,
		// claude -p needs the same Bedrock / Anthropic / proxy plumbing
		// the main session-spawn path uses (applyClaudeEnvSettings
		// pre-populated naozhi's own os.Environ from
		// ~/.claude/settings.json at startup).  Trailing underscore =
		// prefix match, see internal/sysession/env.go's filterEnv.
		// AWS_ is bounded by the same denylist filterClaudeEnv uses for
		// the parent — auth-source vars never make it into naozhi's
		// env in the first place.
		EnvAllowlist: []string{
			"ANTHROPIC_",
			"CLAUDE_",
			"AWS_",
			"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
			"http_proxy", "https_proxy", "no_proxy",
		},
	})
	if err != nil {
		return nil, "", fmt.Errorf("new runner: %w", err)
	}

	tickTimeout := 30 * time.Second
	if v := cfg.Sysession.TickTimeout; v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			slog.Warn("sysession: bad tick_timeout; using default 30s", "err", err, "value", v)
		} else {
			tickTimeout = parsed
		}
	}

	// Translate per-daemon configs.
	daemons := make(map[string]sysession.DaemonRuntimeConfig, len(cfg.Sysession.Daemons))
	for name, dcfg := range cfg.Sysession.Daemons {
		tick := 30 * time.Second
		if dcfg.Tick != "" {
			parsed, err := time.ParseDuration(dcfg.Tick)
			if err != nil {
				slog.Warn("sysession: bad daemon tick; using default 30s",
					"daemon", name, "err", err, "value", dcfg.Tick)
			} else {
				tick = parsed
			}
		}
		specific := sysession.DaemonConfig{}
		if dcfg.MinUserTurns > 0 {
			specific["min_user_turns"] = dcfg.MinUserTurns
		}
		if dcfg.MinRenameInterval != "" {
			parsed, err := time.ParseDuration(dcfg.MinRenameInterval)
			if err != nil {
				slog.Warn("sysession: bad min_rename_interval",
					"daemon", name, "err", err, "value", dcfg.MinRenameInterval)
			} else {
				specific["min_rename_interval"] = parsed
			}
		}
		if dcfg.BatchPerTick > 0 {
			specific["batch_per_tick"] = dcfg.BatchPerTick
		}
		specific["include_group_chat"] = dcfg.IncludeGroupChat

		// attachment-gc knobs (docs/rfc/attachment-gc-daemon.md §5).
		if dcfg.UploadTTL != "" {
			if d, err := time.ParseDuration(dcfg.UploadTTL); err != nil {
				slog.Warn("sysession: bad attachment-gc upload_ttl; using daemon default",
					"daemon", name, "err", err, "value", dcfg.UploadTTL)
			} else {
				specific["upload_ttl"] = d
			}
		}
		if dcfg.RefTTL != "" {
			if d, err := time.ParseDuration(dcfg.RefTTL); err != nil {
				slog.Warn("sysession: bad attachment-gc ref_ttl; using daemon default",
					"daemon", name, "err", err, "value", dcfg.RefTTL)
			} else {
				specific["ref_ttl"] = d
			}
		}
		if dcfg.PerRootCap > 0 {
			specific["per_root_cap"] = dcfg.PerRootCap
		}
		if dcfg.DryRun {
			specific["dry_run"] = true
		}

		// Tick floor for the low-frequency attachment-gc sweeper: a
		// misconfigured short tick would re-walk every attachment dir
		// continuously. GC is fine running hourly at most.
		if name == "attachment-gc" && tick < time.Hour {
			slog.Warn("sysession: attachment-gc tick below 1h floor; clamping",
				"requested", tick, "floor", time.Hour)
			tick = time.Hour
		}

		daemons[name] = sysession.DaemonRuntimeConfig{
			Enabled:    dcfg.Enabled,
			Tick:       tick,
			RunOnStart: dcfg.RunOnStart,
			Specific:   specific,
		}
	}

	mgr, err := sysession.NewManager(sysession.Config{
		Enabled:     true,
		TickTimeout: tickTimeout,
		Runner:      runner,
		Router:      router,
		Daemons:     daemons,
		// attachment-gc daemon sweeps these roots (router default +
		// overrides ∪ project paths). nil-safe inside the lister.
		WorkspaceRoots: workspaceRootLister{router: router, projectMgr: projectMgr},
		// OnRunStarted/OnRunEnded are wired in Step 11 (WS broadcast).
	})
	if err != nil {
		return nil, "", fmt.Errorf("new manager: %w", err)
	}
	return mgr, resolvedWorkDir, nil
}
