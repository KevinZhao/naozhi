// main() 之外的 lifecycle helpers：平台 adapter 构造、解析 helper、磁盘预警、
// sysession.Manager 构造。
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/attachment"
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"
	discordplatform "github.com/naozhi/naozhi/internal/platform/discord"
	"github.com/naozhi/naozhi/internal/platform/feishu"
	slackplatform "github.com/naozhi/naozhi/internal/platform/slack"
	weixinplatform "github.com/naozhi/naozhi/internal/platform/weixin"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/runtelemetry"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/shim"
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

// stateDirWarnMB is the soft ceiling for ~/.naozhi/ total size; see
// docs/ops/disk-budget.md.
const stateDirWarnMB = 500

// warnIfStateDirLarge walks stateDir once at startup and warns if total
// bytes exceed stateDirWarnMB. First-run / permission errors are silent;
// a truncated scan still warns using the partial total as a lower bound.
func warnIfStateDirLarge(stateDir string) {
	warnIfStateDirOver(stateDir, stateDirWarnMB<<20)
}

// warnIfStateDirOver is warnIfStateDirLarge with the threshold in bytes, so
// tests can cross it without writing 500 MiB.
func warnIfStateDirOver(stateDir string, thresholdBytes int64) {
	if stateDir == "" || stateDir == "." {
		return
	}
	bytes, err := osutil.StateDirSize(stateDir)
	truncated := errors.Is(err, osutil.ErrStateDirScanTruncated)
	if err != nil && !truncated {
		return
	}
	if bytes < thresholdBytes {
		return
	}
	slog.Warn("state directory large",
		"path", stateDir, "size_mb", bytes>>20, "threshold_mb", thresholdBytes>>20,
		"truncated", truncated,
		"hint", "prune events/*.log of closed sessions; a session.cwd inside this directory (the default ~/.naozhi/workspace) counts its .naozhi/attachments here too, see the attachments large warning; see docs/ops/disk-budget.md")
}

// attachmentsWarnMB is the soft ceiling for the attachment trees summed over
// every known workspace root; see docs/ops/disk-budget.md.
const attachmentsWarnMB = 500

// Values of attachmentGCMode, logged as the attachment_gc field.
const (
	attachmentGCDisabled = "disabled"
	attachmentGCDryRun   = "dry_run"
	attachmentGCEnabled  = "enabled"
)

// attachmentGCMode reports how cfg runs the attachment-gc daemon. It is
// disabled when sysession is off or the daemon is absent or not enabled.
func attachmentGCMode(cfg *config.Config) string {
	d, ok := cfg.Sysession.Daemons[sysession.DaemonAttachmentGC]
	switch {
	case !cfg.Sysession.Enabled || !ok || !d.Enabled:
		return attachmentGCDisabled
	case d.DryRun:
		return attachmentGCDryRun
	default:
		return attachmentGCEnabled
	}
}

// attachmentsLargeHint tells the operator the next step for gcMode.
func attachmentsLargeHint(gcMode string) string {
	switch gcMode {
	case attachmentGCDryRun:
		return "attachment-gc runs with dry_run: true and removes nothing; review its would-remove results, then set dry_run: false; see docs/ops/disk-budget.md"
	case attachmentGCEnabled:
		return "attachment-gc is on; upload_ttl, ref_ttl or per_root_cap may be too loose for this upload rate; see docs/ops/disk-budget.md"
	default:
		return "attachment-gc is not running; set sysession.enabled and sysession.daemons.attachment-gc.enabled with dry_run: true, review, then set dry_run: false; see docs/ops/disk-budget.md"
	}
}

// warnIfAttachmentsLarge sums <root>/.naozhi/attachments over roots and warns
// once when the total reaches attachmentsWarnMB. A missing or unreadable tree
// counts as 0; a truncated walk adds its partial total and sets truncated.
func warnIfAttachmentsLarge(roots sysession.WorkspaceRootLister, gcMode string) {
	warnIfAttachmentsOver(roots, gcMode, attachmentsWarnMB<<20, osutil.StateDirSize)
}

// warnIfAttachmentsOver is warnIfAttachmentsLarge with the threshold in bytes
// and the tree walk as a parameter, so tests can force a truncated walk.
func warnIfAttachmentsOver(roots sysession.WorkspaceRootLister, gcMode string, thresholdBytes int64, treeSize func(string) (int64, error)) {
	var total, largest int64
	var largestRoot string
	var withTree int
	truncated := false
	for _, root := range roots.KnownWorkspaceRoots() {
		n, err := treeSize(filepath.Join(root, attachment.Dir))
		partial := errors.Is(err, osutil.ErrStateDirScanTruncated)
		if err != nil && !partial {
			continue
		}
		withTree++
		truncated = truncated || partial
		total += n
		if n > largest {
			largest, largestRoot = n, root
		}
	}
	if total < thresholdBytes {
		return
	}
	slog.Warn("attachments large",
		"total_mb", total>>20, "threshold_mb", thresholdBytes>>20,
		"roots", withTree, "largest_root", largestRoot, "largest_mb", largest>>20,
		"truncated", truncated, "attachment_gc", gcMode,
		"hint", attachmentsLargeHint(gcMode))
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
	lay := datadir.ForStore(storePath)
	if root := lay.Root(); root == "" || root == "." {
		home, _ := os.UserHomeDir()
		lay = datadir.FromRoot(filepath.Join(home, ".naozhi"))
	}
	return lay.SysSessionsRoot()
}

// shimManagerConfig is the shim.Manager configuration main starts with.
func shimManagerConfig(cfg *config.Config) shim.ManagerConfig {
	return shim.ManagerConfig{
		StateDir:        osutil.ExpandHome(cfg.Session.Shim.StateDir),
		IdleTimeout:     cfg.ShimIdleTimeout(),
		WatchdogTimeout: cfg.ShimWatchdogTimeout(),
		BufferSize:      cfg.Session.Shim.BufferSize,
		MaxBufBytes:     cfg.ShimMaxBufferBytes(),
		MaxShims:        cfg.Session.Shim.MaxShims,
	}
}

// buildSysessionManager wires sysession.Manager from cfg.Sysession. Returns
// (nil, "", nil) when disabled so the caller's nil guard stays meaningful, and
// (nil, "", err) when enabled but unusable — the caller logs and continues
// without daemons. telemetry is the run-event relay cron shares (#1723).
func buildSysessionManager(cfg *config.Config, router *session.Router,
	projectMgr *project.Manager, defaultWrapper *cli.Wrapper, storePath string,
	telemetry runtelemetry.Broadcaster,
) (*sysession.Manager, string, error) {
	if !cfg.Sysession.Enabled {
		return nil, "", nil
	}

	resolvedWorkDir, err := sysession.EnsureWorkDir(sysSessionsWorkDir(cfg, storePath))
	if err != nil {
		return nil, "", fmt.Errorf("ensure sys-sessions dir: %w", err)
	}

	// Retention for this tree is registered on the shared datadir.Sweeper in
	// main.go (J6). It used to be a single sweep right here, which meant an
	// instance up for weeks never swept again.

	binPath := ""
	if defaultWrapper != nil {
		binPath = defaultWrapper.CLIPath
	}
	backendID := ""
	if defaultWrapper != nil {
		backendID = defaultWrapper.BackendID
	}
	runner, err := sysession.NewRunner(sysession.RunnerConfig{
		BinPath:   binPath,
		BackendID: backendID,
		WorkDir:   resolvedWorkDir,
		Model:     cfg.Sysession.Runner.Model,
		Ledger:    router.Runs().CostLedger(),
	})
	if err != nil {
		return nil, "", fmt.Errorf("new runner: %w", err)
	}

	mgr, err := sysession.NewManager(sysession.Config{
		Enabled:     true,
		TickTimeout: cfg.SysessionTickTimeout(),
		Runner:      runner,
		Router:      router,
		Daemons:     sysessionDaemons(cfg),
		// attachment-gc sweeps these roots; nil-safe inside the lister.
		WorkspaceRoots: workspaceRootLister{router: router, projectMgr: projectMgr},
		Telemetry:      telemetry,
	})
	if err != nil {
		return nil, "", fmt.Errorf("new manager: %w", err)
	}
	return mgr, resolvedWorkDir, nil
}

// sysessionDaemons builds each daemon's runtime config from cfg.Sysession.Daemons
// and the durations config.Load parsed for it.
func sysessionDaemons(cfg *config.Config) map[string]sysession.DaemonRuntimeConfig {
	daemons := make(map[string]sysession.DaemonRuntimeConfig, len(cfg.Sysession.Daemons))
	for name, dcfg := range cfg.Sysession.Daemons {
		durations := cfg.SysessionDaemonDurations(name)
		tick := durations.Tick
		specific := sysession.DaemonConfig{}
		if name == sysession.DaemonAutoTitler {
			if dcfg.MinFirstTurns > 0 {
				specific["min_first_turns"] = dcfg.MinFirstTurns
			}
			if dcfg.MinUserTurns > 0 {
				specific["min_user_turns"] = dcfg.MinUserTurns
			}
			if durations.MinRenameInterval > 0 {
				specific["min_rename_interval"] = durations.MinRenameInterval
			}
			if dcfg.BatchPerTick > 0 {
				specific["batch_per_tick"] = dcfg.BatchPerTick
			}
			specific["include_group_chat"] = dcfg.IncludeGroupChat
		}

		// attachment-gc knobs (docs/rfc/attachment-gc-daemon.md §5).
		if durations.UploadTTL > 0 {
			specific["upload_ttl"] = durations.UploadTTL
		}
		if durations.RefTTL > 0 {
			specific["ref_ttl"] = durations.RefTTL
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
	return daemons
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

// profileBackendNotice is one access profile whose default_backend sends new
// sessions somewhere other than the router default.
type profileBackendNotice struct {
	Profile        string
	DefaultBackend string
	RouterDefault  string
	IsDefault      bool // the profile is default_access_profile
}

// profileDefaultBackendNotices lists, sorted by profile id, the profiles whose
// default_backend is set and differs from routerDefault (the backend startup
// actually bound, not cli.backend). An equal value changes nothing and is
// skipped.
func profileDefaultBackendNotices(profiles map[string]config.AccessProfile, defaultProfile, routerDefault string) []profileBackendNotice {
	var out []profileBackendNotice
	for _, id := range slices.Sorted(maps.Keys(profiles)) {
		be := profiles[id].DefaultBackend
		if be == "" || be == routerDefault {
			continue
		}
		out = append(out, profileBackendNotice{Profile: id, DefaultBackend: be, RouterDefault: routerDefault, IsDefault: id == defaultProfile})
	}
	return out
}

// logProfileDefaultBackends tells the operator at boot which access profiles
// route new sessions off the router default. It is a Warn for
// default_access_profile, which reaches every new session without a pin.
func logProfileDefaultBackends(cfg *config.Config, routerDefault string) {
	const hint = "existing sessions keep their recorded backend; agents[].backend and a project backend override it; see `naozhi config check --effective`"
	for _, n := range profileDefaultBackendNotices(cfg.AccessProfiles, cfg.DefaultAccessProfile, routerDefault) {
		if n.IsDefault {
			slog.Warn("access_profiles["+n.Profile+"].default_backend applies to every new session (it is default_access_profile)",
				"default_backend", n.DefaultBackend, "router_default", n.RouterDefault,
				"scope", "all new sessions without an agent, project or dashboard backend pin", "hint", hint)
			continue
		}
		slog.Info("access_profiles["+n.Profile+"].default_backend applies to new sessions under this profile",
			"default_backend", n.DefaultBackend, "router_default", n.RouterDefault,
			"scope", "new sessions on keys resolved to this profile", "hint", hint)
	}
}

// backendHistoryDir returns the expanded transcript directory a backend keeps its
// sessions in, from backend.Profile.HistoryDir — the same value `naozhi doctor`
// reports (doctor.go's backendHistoryPath).
//
// Wired here rather than repeating the CLI's documented path as a literal: the
// history factories and doctor would otherwise derive one fact two ways, which is
// the shape of #2668 (two derivations of a backend's spawn defaults, one wrong,
// producing a spurious DRIFT on healthy sessions).
//
// "" for an unregistered backend or one with no HistoryDir, which the factories
// already treat as "no fallback history for this backend".
func backendHistoryDir(id string) string {
	p, ok := backend.Get(id)
	if !ok || p.HistoryDir == "" {
		return ""
	}
	return osutil.ExpandHome(p.HistoryDir)
}

// backendHistoryDirs is backendHistoryDir for every registered backend that
// declares a HistoryDir, keyed by backend ID: a new backend's transcripts are
// wired by registering its profile, not by another line here.
func backendHistoryDirs() map[string]string {
	out := map[string]string{}
	for _, p := range backend.All() {
		if dir := backendHistoryDir(p.ID); dir != "" {
			out[p.ID] = dir
		}
	}
	return out
}

// newDataDirSweeper registers the retention passes for the trees that only
// accumulate. Ages are chosen so a pass can never remove a file a live process
// is still appending to:
//
//   - cli-debug/<keyhash>.log is appended for a session's whole life, and age
//     alone cannot prove that life is over: the shim idle timer only runs while
//     naozhi is detached, and exempt sessions or a long session.ttl keep a CLI
//     idle for weeks. shim.Manager.KeyHashFileIsLive keeps any file whose
//     session still has a live shim; the max(7d, 2×idle) age is only the cheap
//     pre-filter that decides which files are worth asking about.
//   - shims/shim-<pid>.log is decided by liveness, not age: shim.LogFileIsLive
//     keeps anything whose pid still resolves. The 24h age is a diagnosis grace
//     period — a shim that just died keeps its log for a day, which is when it
//     is worth reading.
//   - sys-sessions/*.jsonl keeps its configured window (jsonl_max_age).
//   - stdout/stderr, when an init system redirected them into files, are capped
//     by size (log.stdio_max_size) rather than age; see addStdioCaps.
func newDataDirSweeper(cfg *config.Config, layout datadir.Layout, shimMgr *shim.Manager, sysWorkDir string) *datadir.Sweeper {
	idle := cfg.ShimIdleTimeout()
	cliDebugMaxAge := 7 * 24 * time.Hour
	if two := 2 * idle; two > cliDebugMaxAge {
		cliDebugMaxAge = two
	}

	s := datadir.NewSweeper(dataDirSweepInterval)
	cliDebug := datadir.Pass{
		Name:   "cli-debug",
		Dir:    layout.CLIDebugRoot(),
		Ext:    ".log",
		MaxAge: cliDebugMaxAge,
	}
	if shimMgr != nil {
		cliDebug.Keep = shimMgr.KeyHashFileIsLive
	}
	s.Add(cliDebug)
	// shimMgr.StateDir(), never cfg.Session.Shim.StateDir: NewManager applies the
	// ~/.naozhi/shims default to its own copy, so the raw config value is empty in
	// the common case and this pass silently swept nothing. Taking the Manager
	// rather than a string makes that mistake unrepresentable.
	if shimMgr != nil {
		s.Add(datadir.Pass{
			Name:   "shim-logs",
			Dir:    shimMgr.StateDir(),
			Ext:    ".log",
			MaxAge: 24 * time.Hour,
			Keep:   shim.LogFileIsLive,
		})
	}
	// sys-sessions is the one tree that legitimately has no directory: it does
	// not exist when sysession is disabled. Registering it anyway would make an
	// empty Dir look normal for every pass.
	if sysWorkDir != "" {
		s.Add(datadir.Pass{
			Name:   "sys-sessions",
			Dir:    sysWorkDir,
			Ext:    ".jsonl",
			MaxAge: cfg.SysessionJSONLMaxAge(),
		})
	}
	addStdioCaps(s, cfg, os.Stdout, os.Stderr)
	return s
}

// addStdioCaps registers the stdout and stderr size caps. Taking the files
// lets tests hand in regular files; production passes os.Stdout and os.Stderr,
// which the cap leaves alone unless they are O_APPEND regular files.
func addStdioCaps(s *datadir.Sweeper, cfg *config.Config, stdout, stderr *os.File) {
	maxSize := cfg.LogStdioMaxSize()
	if maxSize <= 0 {
		return
	}
	s.AddFunc("stdio-stdout", datadir.StdioTask(stdout, "stdout", maxSize))
	s.AddFunc("stdio-stderr", datadir.StdioTask(stderr, "stderr", maxSize))
}

// dataDirSweepInterval is how often the shared sweeper runs. Hourly: the trees
// it gardens grow at a few files per hour at most, and Sweeper.Run does one pass
// immediately at startup so a long-lived instance is not the only thing that
// gets swept.
const dataDirSweepInterval = time.Hour

// newProjectManager builds the project manager for cfg.Projects; the caller
// checks Root != "" and runs the first Scan.
func newProjectManager(cfg *config.Config, layout datadir.Layout) (*project.Manager, error) {
	return project.NewManager(osutil.ExpandHome(cfg.Projects.Root), project.PlannerDefaults{
		Model:  cfg.Projects.PlannerDefaults.Model,
		Prompt: cfg.Projects.PlannerDefaults.Prompt,
	}, project.WithIncludeRoot(cfg.Projects.IncludeRoot),
		project.WithExclude(cfg.Projects.Exclude),
		project.WithIndexPath(layout.ProjectsIndexPath()))
}
