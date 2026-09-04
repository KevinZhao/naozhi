package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// semverGreater reports a > b as plain vX.Y.Z (pre-release suffixes ignored).
// Unparseable input returns false: never upgrade on an ambiguous version.
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

// NotifyFunc delivers an update notice to an IM channel, best-effort: nil or a
// delivery failure never stops the binary work.
type NotifyFunc func(text string)

// CheckerConfig configures a background auto-update Checker.
type CheckerConfig struct {
	// CurrentVersion is the running binary's version; "dev" is never auto-upgraded.
	CurrentVersion string

	// Mode selects notify / download / auto behaviour.
	Mode Mode

	// Interval is the time between checks (config already clamps the 1h floor).
	Interval time.Duration

	// CheckOnStart runs one check shortly after Run begins.
	CheckOnStart bool

	// Notify, if non-nil, receives update notices (best-effort).
	Notify NotifyFunc

	// Status, if non-nil, receives every state transition for the dashboard;
	// nil disables reporting (a nil *Status is a valid no-op receiver).
	Status *Status
}

// latestRelease is indirected so tests can stub the lookup; tests that swap
// it MUST NOT call t.Parallel().
var latestRelease = LatestRelease

// Checker periodically polls GitHub Releases and reacts per Mode.
type Checker struct {
	cfg CheckerConfig

	// installed is the tag this process already installed, so ticks do not
	// re-download while the old binary is still running.
	installed string

	// checkMu serializes on-demand checks (CheckNow) and guards lastCheckNow.
	// The periodic Run loop does NOT take it; concurrent lookups are harmless.
	checkMu      sync.Mutex
	lastCheckNow time.Time

	// installMu serializes every binary write and guards `installed`. The
	// short-circuit check and doInstall must be ONE atomic decision: two callers
	// both reading "not installed" would both Replace(), and the second Replace
	// copies the new binary over the backup, destroying the rollback artifact.
	installMu sync.Mutex
}

// minOnDemandInterval throttles CheckNow globally — every open browser tab
// calls it, so the floor cannot be per caller.
const minOnDemandInterval = 15 * time.Minute

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

// Run blocks until ctx is cancelled, checking on the configured cadence. A
// failing check logs and the loop continues; it must never take down the gateway.
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
		// Delay so startup is not competing with network I/O and a crash-restart
		// loop on a bad release cannot instantly re-trigger work.
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

// checkOnce performs one poll+react cycle; all errors are logged and swallowed.
func (c *Checker) checkOnce(ctx context.Context) {
	// Auto-replacing a dev build would silently discard a local build.
	if c.cfg.CurrentVersion == "dev" || c.cfg.CurrentVersion == "" {
		slog.Debug("auto-update: skipping check for dev/unknown build")
		return
	}

	// A stuck connection must not pin the goroutine across a whole interval.
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	rel, err := latestRelease(cctx)
	c.cfg.Status.noteCheck(relTag(rel), err)
	if err != nil {
		slog.Warn("auto-update: check failed", "err", err)
		return
	}

	// Strictly greater only: equality/inequality would allow a downgrade
	// attack via a rolled-back "latest" tag.
	if rel.Tag == c.installedTag() {
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
		// Error dropped: doInstall already logged and notified; the return
		// exists for InstallLatest's HTTP caller.
		_ = c.installLocked(cctx, rel, false)
	case ModeAuto:
		_ = c.installLocked(cctx, rel, true)
	}
}

// installedTag reads the installed tag under installMu.
func (c *Checker) installedTag() string {
	c.installMu.Lock()
	defer c.installMu.Unlock()
	return c.installed
}

// installLocked runs doInstall under installMu, re-checking the
// already-installed short-circuit inside the lock — checkOnce's earlier read
// is only an optimisation; this one cannot be raced into a second Replace().
func (c *Checker) installLocked(ctx context.Context, rel *Release, restart bool) error {
	c.installMu.Lock()
	defer c.installMu.Unlock()
	if rel.Tag == c.installed {
		return ErrNothingToDo
	}
	return c.doInstall(ctx, rel, restart)
}

// doInstall downloads, verifies and atomically replaces the binary, optionally
// restarting the service. Failures degrade to a logged warning + notice and
// are also returned (the tick swallows, the dashboard handler reports). Callers
// MUST hold installMu: a second Replace in the staged state destroys the backup.
func (c *Checker) doInstall(ctx context.Context, rel *Release, restart bool) error {
	c.cfg.Status.notePhase(PhaseInstalling, "", nil)

	selfPath, err := SelfPath()
	if err != nil {
		slog.Warn("auto-update: locate running binary failed", "err", err)
		c.cfg.Status.notePhase(PhaseFailed, "", err)
		return err
	}

	tmp, err := os.MkdirTemp("", "naozhi-autoupdate-*")
	if err != nil {
		slog.Warn("auto-update: temp dir failed", "err", err)
		c.cfg.Status.notePhase(PhaseFailed, "", err)
		return err
	}
	defer os.RemoveAll(tmp)

	newBin, err := Download(ctx, rel, tmp)
	if err != nil {
		slog.Warn("auto-update: download/verify failed", "tag", rel.Tag, "err", err)
		c.cfg.Status.notePhase(PhaseFailed, "", err)
		c.notify(fmt.Sprintf("⚠️ naozhi %s 自动下载失败：%v。请手动 `naozhi upgrade`。", rel.Tag, err))
		return err
	}

	backupPath, err := Replace(newBin, selfPath)
	if err != nil {
		// Replace restored the prior binary; the service is unchanged.
		slog.Warn("auto-update: install failed (service unchanged)", "tag", rel.Tag, "err", err)
		c.cfg.Status.notePhase(PhaseFailed, "", err)
		// Usually a permission problem fixed outside this process; drop the
		// cached writability verdict so the dashboard reflects the fix at once.
		invalidatePreflight()
		c.notify(fmt.Sprintf("⚠️ naozhi %s 自动安装失败：%v。请手动 `naozhi upgrade`。", rel.Tag, err))
		return err
	}

	c.installed = rel.Tag
	// PhaseStaged: bytes on disk, only a restart missing — the transition the
	// dashboard exists to surface.
	c.cfg.Status.notePhase(PhaseStaged, rel.Tag, nil)
	slog.Info("auto-update: binary installed", "tag", rel.Tag, "path", selfPath, "restart", restart)

	if !restart {
		c.notify(fmt.Sprintf("✅ naozhi %s 已下载并安装（当前进程仍为 %s）。下次重启生效，或运行 `sudo systemctl restart naozhi`。",
			rel.Tag, c.cfg.CurrentVersion))
		// The backup stays as the manual rollback artifact until a restart.
		_ = backupPath
		return nil
	}

	// No outer ServiceRunning gate here: a stale "not running" read would skip
	// the restart with c.installed already set, stranding a staged binary
	// silently. RestartServiceNoWait is the single authority (and already a
	// no-op when nothing manages us). The backup is DELIBERATELY kept: it is
	// the only rollback artifact if the new binary fails to boot.
	slog.Info("auto-update: triggering self-restart", "tag", rel.Tag, "backup_kept", backupPath)
	c.cfg.Status.notePhase(PhaseRestarting, rel.Tag, nil)
	c.notify(fmt.Sprintf("🔄 naozhi 正在自动升级到 %s 并重启…", rel.Tag))
	if err := RestartServiceNoWait(ctx); err != nil {
		// Installed and verified; only the trigger failed. Do NOT roll back.
		slog.Warn("auto-update: restart trigger failed (binary IS installed)", "tag", rel.Tag, "err", err)
		// Staged (with LastErr), not Failed: Failed would invite a retry of an
		// install that already completed — the backup-destroying path.
		c.cfg.Status.notePhase(PhaseStaged, rel.Tag, err)
		c.notify(fmt.Sprintf("⚠️ naozhi %s 已安装但重启触发失败：%v。请手动 `sudo systemctl restart naozhi`。", rel.Tag, err))
		return err
	}
	// Restart queued; this process is about to receive SIGTERM.
	return nil
}

// notify delivers a notice best-effort.
func (c *Checker) notify(text string) {
	if c.cfg.Notify == nil {
		return
	}
	c.cfg.Notify(text)
}

// relTag safely reads a tag off a possibly-nil Release.
func relTag(rel *Release) string {
	if rel == nil {
		return ""
	}
	return rel.Tag
}

// CheckNow performs one on-demand release lookup and records it in Status,
// filling the cold-start window before the first periodic check. It NEVER
// installs — a GET must not mutate the deployment. Throttled globally under
// checkMu; losers get ErrCheckThrottled and render the existing Status.
func (c *Checker) CheckNow(ctx context.Context) error {
	if c == nil {
		return errors.New("no update checker configured")
	}
	if c.cfg.CurrentVersion == "dev" || c.cfg.CurrentVersion == "" {
		return ErrCheckSkippedDev
	}

	c.checkMu.Lock()
	defer c.checkMu.Unlock()
	if !c.lastCheckNow.IsZero() && time.Since(c.lastCheckNow) < minOnDemandInterval {
		return ErrCheckThrottled
	}
	// Stamp BEFORE the network call so a slow failure still consumes the
	// interval instead of queueing every poll behind a dead endpoint.
	c.lastCheckNow = time.Now()

	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	rel, err := latestRelease(cctx)
	c.cfg.Status.noteCheck(relTag(rel), err)
	if err != nil {
		slog.Debug("auto-update: on-demand check failed", "err", err)
		return err
	}
	slog.Debug("auto-update: on-demand check done", "latest", rel.Tag)
	return nil
}

// ErrCheckThrottled means an on-demand check was declined because one ran
// recently; callers render the current Status.
var ErrCheckThrottled = errors.New("update check throttled")

// ErrCheckSkippedDev means the running build is a dev build.
var ErrCheckSkippedDev = errors.New("update check skipped for dev build")
