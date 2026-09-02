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

	// Status, if non-nil, receives every state transition so the dashboard
	// can render it. Nil disables the reporting entirely and leaves this
	// Checker behaving exactly as it did before Status existed — which is
	// what lets the pre-existing tests in this package stand as a regression
	// gate without modification.
	Status *Status
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

	// checkMu serializes on-demand checks (CheckNow) against each other and
	// guards lastCheckNow. The periodic Run loop does NOT take it: it is a
	// single goroutine on a >=1h cadence, and making it wait behind a
	// dashboard-triggered check would be pointless coupling. Concurrent
	// latestRelease calls are harmless (two reads of the same remote state)
	// and Status has its own lock.
	checkMu      sync.Mutex
	lastCheckNow time.Time

	// installMu serializes everything that writes the binary, and guards
	// `installed`. Before the dashboard could trigger an install this package
	// had a single writer (the Run goroutine) and needed no lock; InstallLatest
	// (install.go) made that false.
	//
	// It covers the `installed` short-circuit as well as doInstall itself, not
	// just the write: the check and the install must be one atomic decision.
	// Splitting them would let two callers both read "not installed yet" and
	// both Replace(), and the SECOND Replace is the one that copies the new
	// binary over the backup and destroys the rollback artifact (RFC §1.3).
	installMu sync.Mutex
}

// minOnDemandInterval throttles CheckNow. The dashboard calls it only to fill
// the cold-start window (see CheckNow), but "only" still means once per open
// browser tab, so the floor has to hold globally rather than per caller.
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
	c.cfg.Status.noteCheck(relTag(rel), err)
	if err != nil {
		slog.Warn("auto-update: check failed", "err", err)
		return
	}

	// R20260602141221-SEC-1: require the remote tag to be strictly greater than
	// the running version. String equality alone would allow a downgrade attack
	// (an adversary pushing v0.0.1 to trigger a rollback to a vulnerable release).
	// semverGreater returns false on parse failure, falling back to conservative
	// "do not upgrade" — same effect as skipping the check.
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
		// Error deliberately dropped: doInstall already logged and notified.
		// The tick loop's contract is to never propagate a failure — the
		// error return exists for InstallLatest, whose caller is an HTTP
		// handler that must report the outcome.
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

// installLocked runs doInstall under installMu, re-checking the already-
// installed short-circuit inside the lock.
//
// checkOnce checks that condition too, before the mode switch, but that read
// is only an optimisation — it can go stale the moment it is taken if the
// dashboard is installing concurrently. The check that MATTERS is this one,
// because it is the one that cannot be raced past into a second Replace()
// (RFC §1.3).
func (c *Checker) installLocked(ctx context.Context, rel *Release, restart bool) error {
	c.installMu.Lock()
	defer c.installMu.Unlock()
	if rel.Tag == c.installed {
		return ErrNothingToDo
	}
	return c.doInstall(ctx, rel, restart)
}

// doInstall downloads, verifies, and atomically replaces the binary. When
// restart is true it also restarts the running service. Every failure mode
// degrades to a logged warning + best-effort notice; the running service is
// never left broken because Replace restores its backup on any swap failure.
//
// Callers MUST hold installMu: this both writes `installed` and performs the
// Replace whose second invocation in the staged state would destroy the backup
// (RFC §1.3). Reach it through installLocked or InstallLatest, never directly.
//
// It returns the failure in addition to logging + notifying it. The two callers
// need different error policies from one pipeline: the background tick swallows
// (a failed check must never disturb the gateway), while the dashboard's apply
// handler has an operator waiting for an answer.
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
		// Replace restores the prior binary on any failure, so the service
		// keeps running the old version. A common cause here is no write
		// permission to the install dir (binary in /usr/local/bin owned by
		// root while the service runs as a normal user) — degrade to a notice.
		slog.Warn("auto-update: install failed (service unchanged)", "tag", rel.Tag, "err", err)
		c.cfg.Status.notePhase(PhaseFailed, "", err)
		// A failure here is usually a permission problem, and the operator's
		// fix (chown/chmod) happens outside this process. Drop the cached
		// writability verdict so the dashboard reflects the fix immediately
		// rather than up to preflightTTL later.
		invalidatePreflight()
		c.notify(fmt.Sprintf("⚠️ naozhi %s 自动安装失败：%v。请手动 `naozhi upgrade`。", rel.Tag, err))
		return err
	}

	// Mark installed so we don't re-download next tick while the old binary
	// is still the one running.
	c.installed = rel.Tag
	// PhaseStaged, not PhaseInstalling: the bytes are on disk and the ONLY
	// thing between the operator and the new version is a restart. This is the
	// transition the dashboard exists to surface — before it, a staged binary
	// was invisible outside the log.
	c.cfg.Status.notePhase(PhaseStaged, rel.Tag, nil)
	slog.Info("auto-update: binary installed", "tag", rel.Tag, "path", selfPath, "restart", restart)

	if !restart {
		c.notify(fmt.Sprintf("✅ naozhi %s 已下载并安装（当前进程仍为 %s）。下次重启生效，或运行 `sudo systemctl restart naozhi`。",
			rel.Tag, c.cfg.CurrentVersion))
		// Keep the backup as a manual rollback artifact until a restart picks
		// up the new binary; a stale .bak is harmless and small.
		_ = backupPath
		return nil
	}

	// R20260602141221-CR-3: do NOT gate the restart on an outer ServiceRunning()
	// check here. That created a TOCTOU window: ServiceRunning() could read
	// "active" at this point but flip to false (or the reverse) before the
	// inner check inside restartSystemdNoWait, and a stale read here that
	// returned false would skip the restart entirely while c.installed was
	// already set — the next tick then silently no-ops (rel.Tag == c.installed),
	// stranding a staged-but-never-restarted binary with no notice, no WARN.
	// RestartServiceNoWait is the SINGLE authority: it is already a no-op when
	// the service is not running (restartSystemdNoWait returns nil early), so
	// the "service not running ⇒ don't start it" semantics are preserved there.
	//
	// In-process self-restart: we ARE the process systemd will kill. Use the
	// fire-and-forget primitive (RestartServiceNoWait), NOT RestartService —
	// the latter polls `is-active`, which at the instant the restart job is
	// queued still sees US as active and would falsely "confirm" success, then
	// delete the backup right before systemd kills us. If the new binary then
	// failed to boot we'd have no rollback artifact. So:
	//   - trigger the restart and return; systemd Restart=always brings the
	//     new binary up.
	//   - DELIBERATELY keep backupPath unconditionally. A stale .bak is harmless
	//     and small, and it is the only rollback artifact if the new binary is
	//     bad. The next successful upgrade's Replace overwrites it (O_TRUNC), so
	//     it does not accumulate.
	slog.Info("auto-update: triggering self-restart", "tag", rel.Tag, "backup_kept", backupPath)
	c.cfg.Status.notePhase(PhaseRestarting, rel.Tag, nil)
	c.notify(fmt.Sprintf("🔄 naozhi 正在自动升级到 %s 并重启…", rel.Tag))
	if err := RestartServiceNoWait(ctx); err != nil {
		// The binary is installed and verified; only the restart trigger
		// failed to enqueue. Do NOT roll back — the operator can restart
		// manually and the backup is still on disk.
		slog.Warn("auto-update: restart trigger failed (binary IS installed)", "tag", rel.Tag, "err", err)
		// Back to Staged rather than Failed: the install SUCCEEDED and the
		// binary is waiting on disk. Reporting Failed here would tell the
		// operator to retry an install that already completed — which in the
		// staged state is the backup-destroying path (see ActionRestart).
		// LastErr still carries why the restart did not happen.
		c.cfg.Status.notePhase(PhaseStaged, rel.Tag, err)
		c.notify(fmt.Sprintf("⚠️ naozhi %s 已安装但重启触发失败：%v。请手动 `sudo systemctl restart naozhi`。", rel.Tag, err))
		return err
	}
	// Restart is queued; this process is about to receive SIGTERM. The
	// "🔄 restarting" notice above is the last one this generation emits.
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

// CheckNow performs a single release lookup on demand and records it in
// Status. It NEVER installs anything — it only learns the latest tag.
//
// Why it exists: the default cadence is 6h with check_on_start disabled, so a
// freshly restarted naozhi knows nothing about available releases for up to six
// hours. That is precisely when an operator is most likely to open the
// dashboard and ask "am I current?", and the honest answer must not be a blank
// space. CheckNow fills that window.
//
// Why it never installs: installing is a decision that belongs to the
// configured Mode (or, from P2, to an explicit operator action). Letting a GET
// request trigger a binary replacement as a side effect would make a read
// endpoint mutate the deployment.
//
// Throttling is global, not per caller: minOnDemandInterval is enforced under
// checkMu, so N dashboard tabs polling simultaneously still produce at most one
// GitHub request per interval. Callers that lose the race get ErrCheckThrottled
// and should simply render the existing Status.
func (c *Checker) CheckNow(ctx context.Context) error {
	if c == nil {
		return errors.New("no update checker configured")
	}
	// A dev build has no meaningful released version to compare against —
	// same rule checkOnce applies, stated here so the endpoint does not reach
	// out to GitHub on developer machines at all.
	if c.cfg.CurrentVersion == "dev" || c.cfg.CurrentVersion == "" {
		return ErrCheckSkippedDev
	}

	c.checkMu.Lock()
	defer c.checkMu.Unlock()
	if !c.lastCheckNow.IsZero() && time.Since(c.lastCheckNow) < minOnDemandInterval {
		return ErrCheckThrottled
	}
	// Stamp BEFORE the network call, not after. An unreachable GitHub can hold
	// this goroutine for the full timeout below; stamping first means a slow
	// failure still consumes the interval instead of letting every subsequent
	// poll queue up behind the same dead endpoint.
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
// recently. Not an error condition for the caller — render current Status.
var ErrCheckThrottled = errors.New("update check throttled")

// ErrCheckSkippedDev means the running build is a dev build, for which there
// is no meaningful release comparison.
var ErrCheckSkippedDev = errors.New("update check skipped for dev build")
