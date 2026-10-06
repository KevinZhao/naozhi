package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/server"
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
	r.bindApply(func(h server.HotConfig) { applied = append(applied, h) })
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
	if len(*applied) != 1 || (*applied)[0].Access == nil || len((*applied)[0].Access.Rules["feishu"].Allowed) != 1 {
		t.Fatalf("apply received %+v", *applied)
	}
	if level.Level() != slog.LevelInfo {
		t.Fatalf("log level moved to %v without a change", level.Level())
	}
	if sha, _ := r.fp.Get(); sha != res.SHA256 || sha == "" {
		t.Fatalf("fingerprint %q not updated to %q", sha, res.SHA256)
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
	// Hot sections still get applied even when nothing hot changed, so a
	// second reload keeps reporting the cli drift against the startup config.
	res, err = r.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 0 || !reflect.DeepEqual(res.RestartRequired, []string{"cli"}) {
		t.Fatalf("second result = %+v", res)
	}
	if len(*applied) != 2 {
		t.Fatalf("apply calls = %d, want 2", len(*applied))
	}
}

func TestConfigReloader_BadFileLeavesStateUntouched(t *testing.T) {
	r, path, applied, level := newReloaderFixture(t)
	shaBefore, _ := r.fp.Get()
	writeConfigFile(t, path, "cli:\n  model: [not a string\nlog:\n  level: debug\n")
	if _, err := r.Reload(context.Background()); err == nil {
		t.Fatal("broken YAML reloaded without error")
	}
	writeConfigFile(t, path, reloadBase+"im_limits:\n  per_chat_daily_usd: -1\n")
	if _, err := r.Reload(context.Background()); err == nil {
		t.Fatal("invalid im_limits reloaded without error")
	}
	if len(*applied) != 0 || level.Level() != slog.LevelInfo {
		t.Fatalf("failed reloads had side effects: applied=%d level=%v", len(*applied), level.Level())
	}
	if sha, _ := r.fp.Get(); sha != shaBefore {
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
