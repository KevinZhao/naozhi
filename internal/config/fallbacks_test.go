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
		{"sysession tick timeout", "sysession:\n  tick_timeout: 5 minutes\n", "sysession.tick_timeout", "fallback", nil},
		{"daemon tick", "sysession:\n  daemons:\n    auto_titler:\n      tick: every minute\n", "sysession.daemons.auto_titler.tick", "fallback", nil},
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
