package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Defaults: unset update block → enabled, mode "download", 6h interval.
func TestUpdateDefaults(t *testing.T) {
	tmpFile := t.TempDir() + "/config.yaml"
	os.WriteFile(tmpFile, []byte("{}"), 0600)

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if !cfg.UpdateEnabled() {
		t.Error("update should default to enabled")
	}
	if cfg.Update.Mode != "download" {
		t.Errorf("default mode = %q, want download", cfg.Update.Mode)
	}
	if cfg.UpdateInterval() != 6*time.Hour {
		t.Errorf("default interval = %v, want 6h", cfg.UpdateInterval())
	}
}

// Explicit disable turns the checker off and short-circuits interval parsing.
func TestUpdateDisabled(t *testing.T) {
	tmpFile := t.TempDir() + "/config.yaml"
	os.WriteFile(tmpFile, []byte(`
update:
  enabled: false
`), 0600)

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.UpdateEnabled() {
		t.Error("update.enabled=false should disable the checker")
	}
}

// An unknown mode is refused, switched on or not.
func TestUpdateUnknownModeRefused(t *testing.T) {
	for _, body := range []string{"update:\n  mode: rocket\n", "update:\n  enabled: false\n  mode: Auto\n"} {
		tmpFile := t.TempDir() + "/config.yaml"
		os.WriteFile(tmpFile, []byte(body), 0600)
		if _, err := Load(tmpFile); err == nil || !strings.Contains(err.Error(), "update.mode") {
			t.Errorf("%q: Load() error = %v, want an update.mode error", body, err)
		}
	}
}

// Interval below the 1h floor is clamped up.
func TestUpdateIntervalFloor(t *testing.T) {
	tmpFile := t.TempDir() + "/config.yaml"
	os.WriteFile(tmpFile, []byte(`
update:
  interval: 5m
`), 0600)

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.UpdateInterval() != time.Hour {
		t.Errorf("sub-floor interval = %v, want clamped to 1h", cfg.UpdateInterval())
	}
}

// A valid interval above the floor passes through.
func TestUpdateIntervalCustom(t *testing.T) {
	tmpFile := t.TempDir() + "/config.yaml"
	os.WriteFile(tmpFile, []byte(`
platforms:
  feishu:
    app_id: cli_x
    app_secret: s
    bot_name: bot
update:
  interval: 12h
  mode: notify
  check_on_start: true
  notify:
    platform: feishu
    chat_id: oc_abc
`), 0600)

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.UpdateInterval() != 12*time.Hour {
		t.Errorf("interval = %v, want 12h", cfg.UpdateInterval())
	}
	if cfg.Update.Mode != "notify" {
		t.Errorf("mode = %q, want notify", cfg.Update.Mode)
	}
	if !cfg.Update.CheckOnStart {
		t.Error("check_on_start should be true")
	}
	if cfg.Update.Notify.Platform != "feishu" || cfg.Update.Notify.ChatID != "oc_abc" {
		t.Errorf("notify target = %+v", cfg.Update.Notify)
	}
}

// An invalid interval string surfaces as a load error.
func TestUpdateIntervalInvalid(t *testing.T) {
	tmpFile := t.TempDir() + "/config.yaml"
	os.WriteFile(tmpFile, []byte(`
update:
  interval: "not-a-duration"
`), 0600)

	if _, err := Load(tmpFile); err == nil {
		t.Fatal("expected error for invalid update.interval")
	}
}

// R20260602141221-CR-1: update.notify.platform typo should return an error
// when the named platform section is absent, matching the existing
// cron.notify_default.platform guard.
func TestUpdateNotifyPlatformValidation(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr bool
		errMsg  string
	}{
		{
			name: "empty_platform_is_legal",
			yaml: `
update:
  notify:
    chat_id: oc_abc
`,
			wantErr: false,
		},
		{
			name: "known_platform_with_section_ok",
			yaml: `
platforms:
  feishu:
    app_id: cli_x
    app_secret: s
    bot_name: bot
update:
  notify:
    platform: feishu
    chat_id: oc_abc
`,
			wantErr: false,
		},
		{
			name: "misconfig_platform_no_section",
			yaml: `
update:
  notify:
    platform: feishu
    chat_id: oc_abc
`,
			wantErr: true,
			errMsg:  "update.notify.platform",
		},
		{
			name: "typo_platform_name",
			yaml: `
update:
  notify:
    platform: feshu
    chat_id: oc_abc
`,
			wantErr: true,
			errMsg:  "update.notify.platform",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			tmpFile := t.TempDir() + "/config.yaml"
			os.WriteFile(tmpFile, []byte(tt.yaml), 0600)
			_, err := Load(tmpFile)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("error %q does not mention %q", err.Error(), tt.errMsg)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}
