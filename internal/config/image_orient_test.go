package config

import (
	"os"
	"strings"
	"testing"
)

// Unset image_orient block → auto-orient defaults ON, model empty.
func TestImageOrientDefaults(t *testing.T) {
	tmpFile := t.TempDir() + "/config.yaml"
	os.WriteFile(tmpFile, []byte("{}"), 0600)

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if !cfg.ImageOrientEnabled() {
		t.Error("image_orient should default to enabled when the block is absent")
	}
	if cfg.ImageOrient.Model != "" {
		t.Errorf("default model = %q, want empty (CLI default)", cfg.ImageOrient.Model)
	}
}

// Explicit disable turns auto-orient off.
func TestImageOrientDisabled(t *testing.T) {
	tmpFile := t.TempDir() + "/config.yaml"
	os.WriteFile(tmpFile, []byte("image_orient:\n  enabled: false\n"), 0600)

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.ImageOrientEnabled() {
		t.Error("image_orient.enabled=false should disable auto-orient")
	}
}

// A configured model is preserved.
func TestImageOrientModelOverride(t *testing.T) {
	tmpFile := t.TempDir() + "/config.yaml"
	os.WriteFile(tmpFile, []byte("image_orient:\n  model: haiku\n"), 0600)

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.ImageOrient.Model != "haiku" {
		t.Errorf("model = %q, want haiku", cfg.ImageOrient.Model)
	}
	// Model override must not flip the default-on enable.
	if !cfg.ImageOrientEnabled() {
		t.Error("setting only model should leave auto-orient enabled")
	}
}

// A malformed model identifier must be rejected by validateConfig.
func TestImageOrientModelRejectsInjection(t *testing.T) {
	tmpFile := t.TempDir() + "/config.yaml"
	os.WriteFile(tmpFile, []byte("image_orient:\n  model: \"--dangerous flag\"\n"), 0600)

	_, err := Load(tmpFile)
	if err == nil {
		t.Fatal("Load() should reject a model string with a space/flag")
	}
	if !strings.Contains(err.Error(), "image_orient.model") {
		t.Errorf("error should name image_orient.model, got: %v", err)
	}
}
