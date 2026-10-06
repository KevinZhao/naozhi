package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/budget"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/imauth"
	"github.com/naozhi/naozhi/internal/server"
	"github.com/naozhi/naozhi/internal/session"
)

func writeConfigFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const reloadBase = "cli:\n  model: sonnet\nlog:\n  level: info\n"

func newReloaderFixture(t *testing.T) (*configReloader, string, *[]server.HotConfig, *slog.LevelVar) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfigFile(t, path, reloadBase)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	level := new(slog.LevelVar)
	level.Set(resolveLogLevel(cfg.Log.Level))
	fp := server.NewConfigFingerprint(cfg.Fingerprint.SHA256, cfg.Fingerprint.LoadedAt)
	r := newConfigReloader(path, cfg, level, fp)
	var applied []server.HotConfig
	r.bindApply(func(h server.HotConfig) { applied = append(applied, h) }, nil, nil)
	return r, path, &applied, level
}

func TestConfigReloader_AppliesHotSections(t *testing.T) {
	r, path, applied, level := newReloaderFixture(t)
	writeConfigFile(t, path, reloadBase+"im_access:\n  platforms:\n    feishu:\n      allowed_users: [ou_1]\n")
	res, err := r.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Applied, []string{"im_access"}) || len(res.RestartRequired) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if len(*applied) != 1 || (*applied)[0].Access == nil || len((*applied)[0].Access.Rules["feishu"].Allowed) != 1 ||
		!reflect.DeepEqual((*applied)[0].Sections, []string{"im_access"}) {
		t.Fatalf("apply received %+v", *applied)
	}
	if level.Level() != slog.LevelInfo {
		t.Fatalf("log level moved to %v without a change", level.Level())
	}
	if sha, _, pending := r.fp.Get(); sha != res.SHA256 || sha == "" || pending != nil {
		t.Fatalf("fingerprint %q (pending %v) not updated to %q", sha, pending, res.SHA256)
	}
}

// im_rate_limit reaches the server as the dispatch.RateLimit the dispatcher
// installs, and only in the reload that changed it.
func TestConfigReloader_AppliesRateLimitOnlyWhenChanged(t *testing.T) {
	r, path, applied, _ := newReloaderFixture(t)
	withLimit := reloadBase + "im_rate_limit:\n  msgs_per_min: 6\n  burst: 2\n"
	writeConfigFile(t, path, withLimit)
	if _, err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*applied) != 1 || (*applied)[0].RateLimit != (dispatch.RateLimit{MsgsPerMin: 6, Burst: 2}) ||
		!reflect.DeepEqual((*applied)[0].Sections, []string{"im_rate_limit"}) {
		t.Fatalf("apply received %+v", *applied)
	}
	// A log.level-only edit must not hand the rate limit over again: the
	// dispatcher would rebuild its buckets and refill every sender's burst.
	writeConfigFile(t, path, strings.Replace(withLimit, "level: info", "level: debug", 1))
	if _, err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, h := range (*applied)[1:] {
		if slices.Contains(h.Sections, "im_rate_limit") {
			t.Fatalf("log.level-only reload applied im_rate_limit again: %+v", h)
		}
	}
}

// Concurrent reloads hold the lock across the file read: a reload waiting
// for another one reads the file as it is once its turn comes, so an older
// read can never be applied over a newer one.
func TestConfigReloader_LoadsUnderLock(t *testing.T) {
	r, path, _, _ := newReloaderFixture(t)
	reading := make(chan struct{}, 1)
	r.load = func(p string) (*config.Config, error) {
		reading <- struct{}{}
		return config.Load(p)
	}
	r.mu.Lock() // stands in for a reload already in progress
	done := make(chan config.ReloadResult)
	go func() {
		res, _ := r.Reload(context.Background())
		done <- res
	}()
	select {
	case <-reading:
		r.mu.Unlock()
		t.Fatal("Reload read config.yaml before taking the lock")
	case <-time.After(200 * time.Millisecond):
	}
	writeConfigFile(t, path, reloadBase+"im_rate_limit:\n  msgs_per_min: 9\n")
	r.mu.Unlock()
	res := <-done
	if !reflect.DeepEqual(res.Applied, []string{"im_rate_limit"}) || r.last.IMRateLimit.MsgsPerMin != 9 {
		t.Fatalf("reload applied %+v (last msgs_per_min=%d), want the newer file", res, r.last.IMRateLimit.MsgsPerMin)
	}
}

// config_sha256 says the process runs that file in full, so a reload that
// leaves restart-only sections keeps the previous fingerprint and lists them;
// reverting the restart-only edit lets the fingerprint advance again.
func TestConfigReloader_FingerprintWaitsForRestartRequired(t *testing.T) {
	r, path, _, _ := newReloaderFixture(t)
	shaStart, atStart, _ := r.fp.Get()
	writeConfigFile(t, path, "cli:\n  model: opus\nlog:\n  level: debug\n")
	if _, err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sha, at, pending := r.fp.Get(); sha != shaStart || !at.Equal(atStart) || !reflect.DeepEqual(pending, []string{"cli"}) {
		t.Fatalf("fingerprint = %q %v pending %v, want the startup one with [cli]", sha, at, pending)
	}
	writeConfigFile(t, path, reloadBase+"im_rate_limit:\n  msgs_per_min: 9\n")
	res, err := r.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sha, _, pending := r.fp.Get(); sha != res.SHA256 || sha == shaStart || pending != nil {
		t.Fatalf("fingerprint = %q pending %v, want %q with nothing pending", sha, pending, res.SHA256)
	}
}

// A reload that drops a running platform's restriction (here a misspelt
// im_access key) reports it, so `naozhi config reload` can exit non-zero.
func TestConfigReloader_ReportsOpenedPlatform(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	slack := "platforms:\n  slack:\n    bot_token: xoxb-test\n"
	writeConfigFile(t, path, slack+"im_access:\n  platforms:\n    slack:\n      allowed_users: [U1]\n")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	r := newConfigReloader(path, cfg, nil, nil)
	var applied []server.HotConfig
	r.bindApply(func(h server.HotConfig) { applied = append(applied, h) }, nil, nil)
	writeConfigFile(t, path, slack+"im_acess:\n  platforms:\n    slack:\n      allowed_users: [U1]\n")
	res, err := r.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.OpenedPlatforms, []string{"slack"}) || !reflect.DeepEqual(res.OpenPlatforms, []string{"slack"}) {
		t.Fatalf("opened = %v open = %v, want [slack] for both", res.OpenedPlatforms, res.OpenPlatforms)
	}
	if len(applied) != 1 {
		t.Fatalf("apply calls = %d, want 1", len(applied))
	}
	if ok, _ := applied[0].Access.Decide("slack", "U2", imauth.Chat); !ok {
		t.Fatal("applied policy still restricts slack; the reload must apply what the file says")
	}
	// A later reload of the same file opens nothing new, but still says the
	// platform is open: SIGHUP may have been the reload that opened it.
	if res, err = r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(res.OpenedPlatforms) != 0 || !reflect.DeepEqual(res.OpenPlatforms, []string{"slack"}) {
		t.Fatalf("second reload opened = %v open = %v, want none and [slack]", res.OpenedPlatforms, res.OpenPlatforms)
	}
}

// A profile the dashboard created is in config.yaml and already live, so a
// reload must not list access_profiles as pending (which would also freeze
// config_sha256); editing that profile in the file afterwards still does.
func TestConfigReloader_RuntimeCreatedProfileIsNotPending(t *testing.T) {
	r, path, applied, _ := newReloaderFixture(t)
	live := map[string]session.AccessProfile{}
	r.bindApply(func(h server.HotConfig) { *applied = append(*applied, h) },
		func() map[string]session.AccessProfile { return live }, nil)
	created := map[string]config.AccessProfile{
		"team":  {DisplayName: "Team", Env: map[string]string{}},
		"vault": {ChipColor: "#d97757", DefaultModel: "sonnet", Env: map[string]string{"ANTHROPIC_AUTH_TOKEN_FILE": filepath.Join(t.TempDir(), "vault.token")}},
	}
	for _, id := range []string{"team", "vault"} {
		ap := created[id]
		if err := config.AppendAccessProfile(path, id, ap); err != nil {
			t.Fatal(err)
		}
		live[id] = session.AccessProfile{DisplayName: ap.DisplayName, ChipColor: ap.ChipColor, DefaultModel: ap.DefaultModel, Env: ap.Env}
	}
	res, err := r.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.RestartRequired) != 0 {
		t.Fatalf("restart_required = %v after creating profiles that are already live", res.RestartRequired)
	}
	if sha, _, pending := r.fp.Get(); sha != res.SHA256 || pending != nil {
		t.Fatalf("fingerprint = %q pending %v, want %q", sha, pending, res.SHA256)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeConfigFile(t, path, strings.Replace(string(body), `"Team"`, `"Team B"`, 1))
	if res, err = r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.RestartRequired, []string{"access_profiles"}) {
		t.Fatalf("restart_required = %v after editing a live profile, want [access_profiles]", res.RestartRequired)
	}
}

// cost.budget caps reach the gate IM and cron share: a lowered cap blocks
// the next check, and a moved day boundary waits for a restart.
func TestConfigReloader_SwapsBudgetLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	capped := func(usd, zone string) string {
		return reloadBase + "cost:\n  budget:\n    per_chat_daily_usd: " + usd + "\n    timezone: " + zone + "\n"
	}
	writeConfigFile(t, path, capped("10", "UTC"))
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	idx := budget.NewIndex(cfg.BudgetLocation(), nil)
	const key = "feishu:group:oc_1:general"
	idx.Add(costledger.Entry{TS: time.Now(), SessionKey: key, Unit: costledger.UnitUSD, Amount: 6})
	gate := budget.NewGate(cfg.BudgetLimits(), idx)
	fp := server.NewConfigFingerprint(cfg.Fingerprint.SHA256, cfg.Fingerprint.LoadedAt)
	r := newConfigReloader(path, cfg, nil, fp)
	r.bindApply(func(server.HotConfig) {}, nil, gate)
	if v := gate.CheckKey(key); v.Blocked {
		t.Fatalf("before reload = %+v, want admitted", v)
	}

	writeConfigFile(t, path, capped("5", "UTC"))
	res, err := r.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Applied, []string{"cost.budget"}) || len(res.RestartRequired) != 0 {
		t.Fatalf("lowered cap result = %+v", res)
	}
	if v := gate.CheckKey(key); !v.Blocked || v.Limit != 5 {
		t.Fatalf("after lowering to 5 = %+v, want blocked", v)
	}

	writeConfigFile(t, path, capped("5", "Asia/Shanghai"))
	if res, err = r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 0 || !reflect.DeepEqual(res.RestartRequired, []string{"cost.budget.timezone"}) {
		t.Fatalf("moved zone result = %+v", res)
	}
	if _, _, pending := fp.Get(); !reflect.DeepEqual(pending, []string{"cost.budget.timezone"}) {
		t.Fatalf("/health pending = %v", pending)
	}
}

// A process that started without a cap has no gate: a cap added by reload
// is restart_required, not reported as applied.
func TestConfigReloader_BudgetWithoutGateWaitsForRestart(t *testing.T) {
	r, path, applied, _ := newReloaderFixture(t)
	writeConfigFile(t, path, reloadBase+"cost:\n  budget:\n    daily_usd: 5\n")
	res, err := r.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 0 || !reflect.DeepEqual(res.RestartRequired, []string{"cost.budget"}) {
		t.Fatalf("result = %+v, want nothing applied and cost.budget pending", res)
	}
	if len(*applied) != 0 {
		t.Fatalf("apply calls = %d, want 0", len(*applied))
	}
}

func TestConfigReloader_LogLevelAndRestartRequiredAgainstBaseline(t *testing.T) {
	r, path, applied, level := newReloaderFixture(t)
	writeConfigFile(t, path, "cli:\n  model: opus\nlog:\n  level: debug\n")
	res, err := r.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Applied, []string{"log.level"}) || !reflect.DeepEqual(res.RestartRequired, []string{"cli"}) {
		t.Fatalf("first result = %+v", res)
	}
	if level.Level() != slog.LevelDebug {
		t.Fatalf("level = %v, want debug", level.Level())
	}
	// A second reload keeps reporting the cli drift against the startup
	// config; with nothing hot changed it applies nothing.
	res, err = r.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 0 || !reflect.DeepEqual(res.RestartRequired, []string{"cli"}) {
		t.Fatalf("second result = %+v", res)
	}
	if len(*applied) != 1 {
		t.Fatalf("apply calls = %d, want 1 (the second reload changed nothing)", len(*applied))
	}
}

func TestConfigReloader_BadFileLeavesStateUntouched(t *testing.T) {
	r, path, applied, level := newReloaderFixture(t)
	shaBefore, _, _ := r.fp.Get()
	writeConfigFile(t, path, "cli:\n  model: [not a string\nlog:\n  level: debug\n")
	if _, err := r.Reload(context.Background()); err == nil {
		t.Fatal("broken YAML reloaded without error")
	}
	writeConfigFile(t, path, reloadBase+"im_rate_limit:\n  msgs_per_min: -1\n")
	if _, err := r.Reload(context.Background()); err == nil {
		t.Fatal("invalid im_rate_limit reloaded without error")
	}
	if len(*applied) != 0 || level.Level() != slog.LevelInfo {
		t.Fatalf("failed reloads had side effects: applied=%d level=%v", len(*applied), level.Level())
	}
	if sha, _, _ := r.fp.Get(); sha != shaBefore {
		t.Fatal("fingerprint moved on a failed reload")
	}
	if r.last != r.baseline {
		t.Fatal("last config replaced on a failed reload")
	}
}

func TestConfigReloader_UnboundRefuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfigFile(t, path, reloadBase)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	r := newConfigReloader(path, cfg, nil, nil)
	if _, err := r.Reload(context.Background()); err != errReloadNotBound {
		t.Fatalf("err = %v, want errReloadNotBound", err)
	}
}

// SIGHUP reloads and keeps the loop running; the first other signal shuts
// down once and ends it, so a later SIGHUP is not handled.
func TestSignalLoop_HUPReloadsNeverShutsDown(t *testing.T) {
	t.Parallel()
	ch := make(chan os.Signal, 4)
	ch <- syscall.SIGHUP
	ch <- syscall.SIGHUP
	ch <- syscall.SIGTERM
	ch <- syscall.SIGHUP
	close(ch)
	reloads := 0
	var shutdowns []string
	signalLoop(ch, func() { reloads++ }, func(reason string) { shutdowns = append(shutdowns, reason) })
	if reloads != 2 || !reflect.DeepEqual(shutdowns, []string{"signal:" + syscall.SIGTERM.String()}) {
		t.Fatalf("reloads = %d, shutdowns = %q; want 2 reloads then one SIGTERM shutdown", reloads, shutdowns)
	}
}

// The process-wide logger setupLogging installs follows the LevelVar it
// returns, which is what a log.level reload sets.
func TestSetupLogging_DefaultFollowsLevelVar(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	level := setupLogging(&config.Config{Log: config.LogConfig{Level: "info"}})
	ctx := context.Background()
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		t.Fatal("debug enabled at level info")
	}
	level.Set(slog.LevelDebug)
	if !slog.Default().Enabled(ctx, slog.LevelDebug) {
		t.Fatal("default logger ignores the LevelVar: a log.level reload would not take effect")
	}
}
