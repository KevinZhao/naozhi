// main() 之外的 lifecycle helpers：平台 adapter 构造、解析 helper、磁盘预警、
// sysession.Manager 构造。
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

// initPlatforms constructs each configured IM platform adapter; it starts no
// goroutines and touches no globals. stt lets Feishu accept voice messages.
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
// docs/ops/disk-budget.md.
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
// prefixed with "…" so a grep on full IDs does not match.
func chatIDSuffix(id string) string {
	if id == "" {
		return ""
	}
	if len(id) <= 8 {
		return id
	}
	return "…" + id[len(id)-8:]
}

// logWebhookEndpoints logs the webhook URLs operators paste into the IM vendor
// console; platforms without a webhook route (feishu websocket mode) are skipped.
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
			// Route is only exposed when not using socket mode.
			if cfg.Platforms.Slack != nil && cfg.Platforms.Slack.AppToken == "" {
				slog.Info("platform webhook endpoint", "platform", name, "path", "/webhook/slack", "addr", addr)
			}
		case "weixin":
			slog.Info("platform webhook endpoint", "platform", name, "path", "/webhook/weixin", "addr", addr)
		}
	}
}

// workspaceRootLister unions the attachment-gc daemon's workspace roots
// (router default + per-chat overrides, bound project paths), normalised and
// deduped so one directory reached via two strings is swept once. Either
// source may be nil (docs/rfc/attachment-gc-daemon.md §4.4).
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
	// EvalSymlinks failures (dir absent) fall back to the abs form so a
	// not-yet-created root is still swept once it exists.
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		if p == "" {
			continue
		}
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

// sysSessionsWorkDir resolves the cwd for all naozhi-internal one-off CLI
// invocations (sysession daemons + image-orient vision runner): config
// override, else dataDir/sys-sessions/, else ~/.naozhi/sys-sessions. Both
// consumers MUST share it — it is the history panel's SkipWorkspace filter
// target, so JSONLs landing anywhere else leak into the history list.
func sysSessionsWorkDir(cfg *config.Config, storePath string) string {
	if wd := osutil.ExpandHome(cfg.Sysession.Runner.WorkDir); wd != "" {
		return wd
	}
	base := filepath.Dir(storePath)
	if base == "" || base == "." {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".naozhi")
	}
	return filepath.Join(base, "sys-sessions")
}

// buildSysessionManager wires sysession.Manager from cfg.Sysession. Returns
// (nil, "", nil) when disabled so the caller's nil guard stays meaningful, and
// (nil, "", err) when enabled but unusable — the caller logs and continues
// without daemons. Telemetry is wired later via Manager.SetTelemetry (#1723).
func buildSysessionManager(cfg *config.Config, router *session.Router,
	projectMgr *project.Manager, defaultWrapper *cli.Wrapper, storePath string,
) (*sysession.Manager, string, error) {
	if !cfg.Sysession.Enabled {
		return nil, "", nil
	}

	resolvedWorkDir, err := sysession.EnsureWorkDir(sysSessionsWorkDir(cfg, storePath))
	if err != nil {
		return nil, "", fmt.Errorf("ensure sys-sessions dir: %w", err)
	}

	// Startup sweep is non-fatal. Default 7 days when unset; "0" disables.
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

	binPath := ""
	if defaultWrapper != nil {
		binPath = defaultWrapper.CLIPath
	}
	runner, err := sysession.NewRunner(sysession.RunnerConfig{
		BinPath: binPath,
		WorkDir: resolvedWorkDir,
		Model:   cfg.Sysession.Runner.Model,
		Ledger:  router.CostLedger(),
		// Same Bedrock/Anthropic/proxy plumbing as session spawns. Trailing
		// underscore = prefix match. AWS_ auth-source vars never reach naozhi's
		// env in the first place (filterClaudeEnv denylist).
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
		if name == sysession.DaemonAutoTitler {
			if dcfg.MinFirstTurns > 0 {
				specific["min_first_turns"] = dcfg.MinFirstTurns
			}
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
		}

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

		// A short tick would re-walk every attachment dir continuously.
		if name == sysession.DaemonAttachmentGC && tick < sysession.AttachmentGCMinTick {
			slog.Warn("sysession: attachment-gc tick below floor; clamping",
				"requested", tick, "floor", sysession.AttachmentGCMinTick)
			tick = sysession.AttachmentGCMinTick
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
		// attachment-gc sweeps these roots; nil-safe inside the lister.
		WorkspaceRoots: workspaceRootLister{router: router, projectMgr: projectMgr},
	})
	if err != nil {
		return nil, "", fmt.Errorf("new manager: %w", err)
	}
	return mgr, resolvedWorkDir, nil
}

// absConfigPath resolves the -config flag to an absolute path so the
// access-profile create endpoint writes to a stable target; falls back to the
// original value rather than "" (which would disable the endpoint).
func absConfigPath(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(osutil.ExpandHome(p)); err == nil {
		return abs
	}
	return p
}

// buildAccessProfiles translates config.AccessProfile into the session-layer
// view (session must not import config). Nil for an empty map keeps every
// session on the global baseline. Env is copied verbatim: *_FILE expands at
// spawn time and the shim's filterShimEnv re-gates every entry.
func buildAccessProfiles(in map[string]config.AccessProfile) map[string]session.AccessProfile {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]session.AccessProfile, len(in))
	for id, ap := range in {
		out[id] = session.AccessProfile{
			DisplayName:    ap.DisplayName,
			ChipColor:      ap.ChipColor,
			Env:            ap.Env,
			DefaultModel:   ap.DefaultModel,
			DefaultBackend: ap.DefaultBackend,
		}
	}
	return out
}
