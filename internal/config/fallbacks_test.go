package config

import (
	"testing"
	"time"
)

// Each value Load replaces instead of refusing is reported, with the default
// it runs with, so config check can fail on it (#2897 C4). The process still
// starts on every one of them.
func TestLoad_ReportsReplacedValues(t *testing.T) {
	cases := []struct {
		name, body, key, action string
		check                   func(*Config) bool
	}{
		{"cron time zone", "cron:\n  timezone: Mars/Olympus\n", "cron.timezone", "fallback",
			func(c *Config) bool { return c.ParseCronTimezone() == time.Local }},
		{"update mode", "update:\n  mode: dowload\n", "update.mode", "fallback",
			func(c *Config) bool { return c.Update.Mode == "download" }},
		{"update interval floor", "update:\n  interval: 10m\n", "update.interval", "clamped",
			func(c *Config) bool { return c.cachedInterval == time.Hour }},
		{"cron jitter cap", "cron:\n  jitter_max: 1h\n", "cron.jitter_max", "clamped",
			func(c *Config) bool { return c.cachedJitterMax == cronJitterMaxHardCap }},
		{"sysession tick timeout", "sysession:\n  tick_timeout: 5 minutes\n", "sysession.tick_timeout", "fallback",
			func(c *Config) bool { return c.SysessionTickTimeout() == 30*time.Second }},
		{"daemon tick", "sysession:\n  daemons:\n    auto_titler:\n      tick: every minute\n", "sysession.daemons.auto_titler.tick", "fallback",
			func(c *Config) bool { return c.SysessionDaemonDurations("auto_titler").Tick == 30*time.Second }},
		{"shim idle timeout", "session:\n  shim:\n    idle_timeout: 4 hours\n", "session.shim.idle_timeout", "fallback",
			func(c *Config) bool { return c.ShimIdleTimeout() == 4*time.Hour }},
		{"shim idle timeout zero", "session:\n  shim:\n    idle_timeout: \"0\"\n", "session.shim.idle_timeout", "fallback",
			func(c *Config) bool { return c.ShimIdleTimeout() == 4*time.Hour }},
		{"shim watchdog negative", "session:\n  shim:\n    disconnect_watchdog: -5m\n", "session.shim.disconnect_watchdog", "fallback",
			func(c *Config) bool { return c.ShimWatchdogTimeout() == 30*time.Minute }},
		{"shim buffer bytes", "session:\n  shim:\n    max_buffer_bytes: abc\n", "session.shim.max_buffer_bytes", "fallback",
			func(c *Config) bool { return c.ShimMaxBufferBytes() == 50<<20 }},
		{"shim buffer bytes zero", "session:\n  shim:\n    max_buffer_bytes: 0MB\n", "session.shim.max_buffer_bytes", "fallback",
			func(c *Config) bool { return c.ShimMaxBufferBytes() == 50<<20 }},
		{"daemon tick zero", "sysession:\n  daemons:\n    auto_titler:\n      tick: 0s\n", "sysession.daemons.auto_titler.tick", "fallback",
			func(c *Config) bool { return c.SysessionDaemonDurations("auto_titler").Tick == 30*time.Second }},
		{"min rename interval", "sysession:\n  daemons:\n    auto_titler:\n      min_rename_interval: 1 hour\n",
			"sysession.daemons.auto_titler.min_rename_interval", "fallback",
			func(c *Config) bool { return c.SysessionDaemonDurations("auto_titler").MinRenameInterval == 0 }},
		{"upload ttl", "sysession:\n  daemons:\n    attachment_gc:\n      upload_ttl: 30 days\n",
			"sysession.daemons.attachment_gc.upload_ttl", "fallback",
			func(c *Config) bool { return c.SysessionDaemonDurations("attachment_gc").UploadTTL == 0 }},
		{"ref ttl negative", "sysession:\n  daemons:\n    attachment_gc:\n      ref_ttl: -1h\n",
			"sysession.daemons.attachment_gc.ref_ttl", "fallback",
			func(c *Config) bool { return c.SysessionDaemonDurations("attachment_gc").RefTTL == 0 }},
		{"jsonl max age", "sysession:\n  runner:\n    jsonl_max_age: 7d\n", "sysession.runner.jsonl_max_age", "fallback",
			func(c *Config) bool { return c.SysessionJSONLMaxAge() == 7*24*time.Hour }},
		{"jsonl max age negative", "sysession:\n  runner:\n    jsonl_max_age: -1h\n", "sysession.runner.jsonl_max_age", "fallback",
			func(c *Config) bool { return c.SysessionJSONLMaxAge() == 7*24*time.Hour }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diags, cfg, err := collectLoadDiags(t, writeCfg(t, tc.body))
			if err != nil {
				t.Fatalf("Load refused the config: %v (it must start with a default)", err)
			}
			var hit bool
			for _, d := range diags {
				if d.Layer == "config-invalid" && d.Key == tc.key && d.Action == tc.action {
					hit = true
				}
			}
			if !hit {
				t.Errorf("no config-invalid/%s diag for %s; got %+v", tc.action, tc.key, diags)
			}
			if tc.check != nil && !tc.check(cfg) {
				t.Errorf("%s did not fall back to its default", tc.key)
			}
		})
	}
}

// Valid values, and the Local zone in any case, are not reported.
func TestLoad_ValidValuesAreNotReported(t *testing.T) {
	body := "cron:\n  timezone: local\n  jitter_max: 5m\nupdate:\n  mode: notify\n  interval: 6h\n" +
		"sysession:\n  tick_timeout: 45s\n  daemons:\n    auto_titler:\n      tick: 1m\n"
	diags, _, err := collectLoadDiags(t, writeCfg(t, body))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diags {
		if d.Layer == "config-invalid" {
			t.Errorf("valid config reported %+v", d)
		}
	}
	diags, _, err = collectLoadDiags(t, writeCfg(t, "cron:\n  timezone: Asia/Shanghai\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diags {
		if d.Layer == "config-invalid" {
			t.Errorf("a real zone reported %+v", d)
		}
	}
}

// Usable values reach the accessors as written; unset ones get the defaults
// main used to hard-code, and a zero-value Config (no Load) reads the same.
func TestLoad_FallbackValuesParsedOnce(t *testing.T) {
	body := "session:\n  shim:\n    idle_timeout: 2h\n    disconnect_watchdog: 45m\n    max_buffer_bytes: 1gb\n" +
		"sysession:\n  tick_timeout: 45s\n  runner:\n    jsonl_max_age: \"0\"\n  daemons:\n" +
		"    auto_titler:\n      tick: 1m\n      min_rename_interval: 10m\n" +
		"    attachment_gc:\n      upload_ttl: \"0\"\n      ref_ttl: 720h\n"
	diags, cfg, err := collectLoadDiags(t, writeCfg(t, body))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diags {
		if d.Layer == "config-invalid" {
			t.Errorf("valid value reported %+v", d)
		}
	}
	at, gc := cfg.SysessionDaemonDurations("auto_titler"), cfg.SysessionDaemonDurations("attachment_gc")
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"ShimIdleTimeout", cfg.ShimIdleTimeout(), 2 * time.Hour},
		{"ShimWatchdogTimeout", cfg.ShimWatchdogTimeout(), 45 * time.Minute},
		{"ShimMaxBufferBytes", cfg.ShimMaxBufferBytes(), int64(1 << 30)},
		{"SysessionTickTimeout", cfg.SysessionTickTimeout(), 45 * time.Second},
		{"SysessionJSONLMaxAge (\"0\" turns the sweep off)", cfg.SysessionJSONLMaxAge(), time.Duration(0)},
		{"auto_titler tick", at.Tick, time.Minute},
		{"auto_titler min_rename_interval", at.MinRenameInterval, 10 * time.Minute},
		{"attachment_gc tick (unset)", gc.Tick, 30 * time.Second},
		{"attachment_gc upload_ttl (\"0\" = daemon default)", gc.UploadTTL, time.Duration(0)},
		{"attachment_gc ref_ttl", gc.RefTTL, 720 * time.Hour},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

	for name, c := range map[string]*Config{"empty file": mustLoad(t, ""), "zero value": {}} {
		if c.ShimIdleTimeout() != 4*time.Hour || c.ShimWatchdogTimeout() != 30*time.Minute ||
			c.ShimMaxBufferBytes() != 50<<20 || c.SysessionTickTimeout() != 30*time.Second ||
			c.SysessionJSONLMaxAge() != 7*24*time.Hour || c.SysessionDaemonDurations("x").Tick != 30*time.Second {
			t.Errorf("%s: defaults not applied: idle=%v watchdog=%v buf=%d tick_timeout=%v jsonl=%v tick=%v", name,
				c.ShimIdleTimeout(), c.ShimWatchdogTimeout(), c.ShimMaxBufferBytes(), c.SysessionTickTimeout(),
				c.SysessionJSONLMaxAge(), c.SysessionDaemonDurations("x").Tick)
		}
	}
}

func mustLoad(t *testing.T, body string) *Config {
	t.Helper()
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestParseByteSize(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"50MB", 50 << 20, true},
		{"1gb", 1 << 30, true},
		{" 512 kb ", 512 << 10, true},
		{"4096B", 4096, true},
		{"4096", 4096, true},
		{"abc", 0, false},
		{"1.5GB", 0, false},
		{"50M", 0, false},
		{"9223372036854775807GB", 0, false},
	} {
		got, ok := parseByteSize(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("parseByteSize(%q) = %d, %v; want %d, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}
