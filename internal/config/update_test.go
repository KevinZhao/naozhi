package config

import (
	"os"
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

// Unknown mode falls back to download (warn, not fail).
func TestUpdateUnknownModeFallsBack(t *testing.T) {
	tmpFile := t.TempDir() + "/config.yaml"
	os.WriteFile(tmpFile, []byte(`
update:
  mode: rocket
`), 0600)

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Update.Mode != "download" {
		t.Errorf("unknown mode = %q, want download fallback", cfg.Update.Mode)
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
