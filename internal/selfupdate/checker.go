package selfupdate

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// semverGreater returns true when a is strictly greater than b using a simple
// vX.Y.Z comparison. No external dependency is introduced. Pre-release suffixes
// (e.g. "-rc.1") are ignored — release tags in this project are always plain
// vX.Y.Z. If either tag cannot be parsed the function returns false (conservative:
// do not upgrade on ambiguous version strings).
func semverGreater(a, b string) bool {
	pa, ok1 := parseSemver(a)
	pb, ok2 := parseSemver(b)
	if !ok1 || !ok2 {
		return false
	}
	if pa[0] != pb[0] {
		return pa[0] > pb[0]
	}
	if pa[1] != pb[1] {
		return pa[1] > pb[1]
	}
	return pa[2] > pb[2]
}

// parseSemver parses "vX.Y.Z" (with an optional leading "v") into [3]int.
// Returns (zero, false) on any parse failure.
func parseSemver(s string) ([3]int, bool) {
	s = strings.TrimPrefix(s, "v")
	// Strip pre-release suffix if present (e.g. "1.2.3-rc.1" → "1.2.3").
	if idx := strings.IndexByte(s, '-'); idx >= 0 {
		s = s[:idx]
	}
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// Mode selects what the Checker does when it finds a newer release.
type Mode string

const (
	// ModeNotify logs and (if a NotifyFunc is set) sends an IM notice only.
	ModeNotify Mode = "notify"
	// ModeDownload downloads + verifies + atomically replaces the binary but
	// does NOT restart the service — the new binary applies on next restart.
	ModeDownload Mode = "download"
	// ModeAuto downloads + verifies + replaces AND restarts the service.
	ModeAuto Mode = "auto"
)

// ParseMode maps a config string to a Mode, defaulting to ModeDownload for
// anything unrecognized (config validation already warns on unknown values).
func ParseMode(s string) Mode {
	switch Mode(s) {
	case ModeNotify:
		return ModeNotify
	case ModeAuto:
		return ModeAuto
	default:
		return ModeDownload
	}
}

// NotifyFunc delivers a human-readable update notice to an IM channel. The
// Checker calls it best-effort: a nil func or a delivery failure never stops
// the binary work. Implementations should not block longer than a few seconds.
type NotifyFunc func(text string)

// CheckerConfig configures a background auto-update Checker.
type CheckerConfig struct {
	// CurrentVersion is the running binary's version (main.version). A "dev"
	// build is never auto-upgraded (no released tag to compare meaningfully).
	CurrentVersion string

	// Mode selects notify / download / auto behaviour.
	Mode Mode

	// Interval is the time between checks. Already clamped to a >=1h floor
	// by config loading; the Checker does not re-validate.
	Interval time.Duration

	// CheckOnStart runs one check ~immediately after Run begins instead of
	// waiting a full Interval.
	CheckOnStart bool

	// Notify, if non-nil, receives update notices (best-effort).
	Notify NotifyFunc
}

// latestRelease is indirected so checker tests can stub the release lookup
// without reaching GitHub. Production wiring is the real LatestRelease.
//
// Test hygiene: mutable package state with no lock. Tests that swap it MUST
// NOT call t.Parallel(), matching the systemdUnitActive convention in this
// package.
var latestRelease = LatestRelease

// Checker periodically polls GitHub Releases and reacts per Mode. It owns no
// global state and is safe to run as a single goroutine off main.
type Checker struct {
	cfg CheckerConfig

	// installed records the tag this process already downloaded+installed in
	// ModeDownload, so repeated ticks don't re-download the same release while
	// CurrentVersion (the still-running old binary) stays unchanged.
	installed string
}

// NewChecker builds a Checker. Returns nil when the config is unusable
// (interval <= 0), so callers can simply skip Run.
func NewChecker(cfg CheckerConfig) *Checker {
	if cfg.Interval <= 0 {
		return nil
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeDownload
	}
	return &Checker{cfg: cfg}
}

// Run blocks until ctx is cancelled, checking on the configured cadence.
// A panic-free, error-swallowing loop: a single failing check logs and the
// loop continues — an unreachable GitHub or a transient download error must
// never take down the gateway it runs inside.
func (c *Checker) Run(ctx context.Context) {
	if c == nil {
		return
	}
	slog.Info("auto-update checker started",
		"mode", c.cfg.Mode,
		"interval", c.cfg.Interval.String(),
		"check_on_start", c.cfg.CheckOnStart,
		"current_version", c.cfg.CurrentVersion)

	if c.cfg.CheckOnStart {
		// Small delay so startup isn't competing with the first check's
		// network I/O, and a crash-restart loop on a bad release can't
		// instantly re-trigger work.
		// R20260602141221-PERF-7: use NewTimer+Stop instead of time.After so
		// the timer is reclaimed promptly when ctx is cancelled before it fires.
		startDelay := time.NewTimer(30 * time.Second)
		defer startDelay.Stop()
		select {
		case <-ctx.Done():
			return
		case <-startDelay.C:
			c.checkOnce(ctx)
		}
	}

	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("auto-update checker stopped")
			return
		case <-ticker.C:
			c.checkOnce(ctx)
		}
	}
}

// checkOnce performs one poll+react cycle. All errors are logged and
// swallowed; this method never panics out to the loop.
func (c *Checker) checkOnce(ctx context.Context) {
	// A dev build has no meaningful released version to compare against, and
	// auto-replacing it would silently discard a local build. Skip.
	if c.cfg.CurrentVersion == "dev" || c.cfg.CurrentVersion == "" {
		slog.Debug("auto-update: skipping check for dev/unknown build")
		return
	}

	// Bound a single cycle so a stuck connection cannot pin the goroutine
	// across a whole interval.
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	rel, err := latestRelease(cctx)
	if err != nil {
		slog.Warn("auto-update: check failed", "err", err)
		return
	}

	// R20260602141221-SEC-1: require the remote tag to be strictly greater than
	// the running version. String equality alone would allow a downgrade attack
	// (an adversary pushing v0.0.1 to trigger a rollback to a vulnerable release).
	// semverGreater returns false on parse failure, falling back to conservative
	// "do not upgrade" — same effect as skipping the check.
	if rel.Tag == c.installed {
		slog.Debug("auto-update: already installed this release", "tag", rel.Tag)
		return
	}
	if !semverGreater(rel.Tag, c.cfg.CurrentVersion) {
		slog.Debug("auto-update: latest is not newer than current",
			"latest", rel.Tag, "current", c.cfg.CurrentVersion)
		return
	}

	slog.Info("auto-update: newer release found",
		"current", c.cfg.CurrentVersion, "latest", rel.Tag, "mode", c.cfg.Mode)

	switch c.cfg.Mode {
	case ModeNotify:
		c.notify(fmt.Sprintf("🆕 naozhi %s 可用（当前 %s）。运行 `naozhi upgrade` 升级。",
			rel.Tag, c.cfg.CurrentVersion))
	case ModeDownload:
		c.doInstall(cctx, rel, false)
	case ModeAuto:
		c.doInstall(cctx, rel, true)
	}
}

// doInstall downloads, verifies, and atomically replaces the binary. When
// restart is true it also restarts the running service. Every failure mode
// degrades to a logged warning + best-effort notice; the running service is
// never left broken because Replace restores its backup on any swap failure.
func (c *Checker) doInstall(ctx context.Context, rel *Release, restart bool) {
	selfPath, err := SelfPath()
	if err != nil {
		slog.Warn("auto-update: locate running binary failed", "err", err)
		return
	}

	tmp, err := os.MkdirTemp("", "naozhi-autoupdate-*")
	if err != nil {
		slog.Warn("auto-update: temp dir failed", "err", err)
		return
	}
	defer os.RemoveAll(tmp)

	newBin, err := Download(ctx, rel, tmp)
	if err != nil {
		slog.Warn("auto-update: download/verify failed", "tag", rel.Tag, "err", err)
		c.notify(fmt.Sprintf("⚠️ naozhi %s 自动下载失败：%v。请手动 `naozhi upgrade`。", rel.Tag, err))
		return
	}

	backupPath, err := Replace(newBin, selfPath)
	if err != nil {
		// Replace restores the prior binary on any failure, so the service
		// keeps running the old version. A common cause here is no write
		// permission to the install dir (binary in /usr/local/bin owned by
		// root while the service runs as a normal user) — degrade to a notice.
		slog.Warn("auto-update: install failed (service unchanged)", "tag", rel.Tag, "err", err)
		c.notify(fmt.Sprintf("⚠️ naozhi %s 自动安装失败：%v。请手动 `naozhi upgrade`。", rel.Tag, err))
		return
	}

	// Mark installed so we don't re-download next tick while the old binary
	// is still the one running.
	c.installed = rel.Tag
	slog.Info("auto-update: binary installed", "tag", rel.Tag, "path", selfPath, "restart", restart)

	if !restart {
		c.notify(fmt.Sprintf("✅ naozhi %s 已下载并安装（当前进程仍为 %s）。下次重启生效，或运行 `sudo systemctl restart naozhi`。",
			rel.Tag, c.cfg.CurrentVersion))
		// Keep the backup as a manual rollback artifact until a restart picks
		// up the new binary; a stale .bak is harmless and small.
		_ = backupPath
		return
	}

	if !ServiceRunning() {
		// R20260602141221-CR-2: do NOT remove the backup here. ServiceRunning()
		// returning false during a ModeAuto update may mean the previous restart
		// is still in-flight (SIGTERM sent, systemd shows "deactivating"). Deleting
		// the only rollback artifact in that window leaves the system unrecoverable
		// if the new binary fails to boot. Stale .bak files are harmless and small;
		// the next successful Replace call overwrites them (O_TRUNC).
		slog.Info("auto-update: service not running, skipping restart")
		c.notify(fmt.Sprintf("✅ naozhi %s 已安装。服务未在运行，手动启动以生效。", rel.Tag))
		_ = backupPath
		return
	}

	// In-process self-restart: we ARE the process systemd will kill. Use the
	// fire-and-forget primitive (RestartServiceNoWait), NOT RestartService —
	// the latter polls `is-active`, which at the instant the restart job is
	// queued still sees US as active and would falsely "confirm" success, then
	// delete the backup right before systemd kills us. If the new binary then
	// failed to boot we'd have no rollback artifact. So:
	//   - trigger the restart and return; systemd Restart=always brings the
	//     new binary up.
	//   - DELIBERATELY keep backupPath. A stale .bak is harmless and small,
	//     and it is the only rollback artifact if the new binary is bad. The
	//     next successful upgrade's Replace overwrites it (O_TRUNC), so it
	//     does not accumulate.
	slog.Info("auto-update: triggering self-restart", "tag", rel.Tag, "backup_kept", backupPath)
	c.notify(fmt.Sprintf("🔄 naozhi 正在自动升级到 %s 并重启…", rel.Tag))
	if err := RestartServiceNoWait(ctx); err != nil {
		// The binary is installed and verified; only the restart trigger
		// failed to enqueue. Do NOT roll back — the operator can restart
		// manually and the backup is still on disk.
		slog.Warn("auto-update: restart trigger failed (binary IS installed)", "tag", rel.Tag, "err", err)
		c.notify(fmt.Sprintf("⚠️ naozhi %s 已安装但重启触发失败：%v。请手动 `sudo systemctl restart naozhi`。", rel.Tag, err))
		return
	}
	// Restart is queued; this process is about to receive SIGTERM. The
	// "🔄 restarting" notice above is the last one this generation emits.
}

// notify delivers a notice best-effort.
func (c *Checker) notify(text string) {
	if c.cfg.Notify == nil {
		return
	}
	c.cfg.Notify(text)
}
