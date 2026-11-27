package config

import (
	"os"
	"strings"
	"testing"
)

// image_orient defaults ON with the CLI's model; enabled: false turns it off,
// and setting only a model keeps it on.
func TestImageOrientConfig(t *testing.T) {
	for _, tc := range []struct {
		body    string
		enabled bool
		model   string
	}{
		{"{}", true, ""},
		{"image_orient:\n  enabled: false\n", false, ""},
		{"image_orient:\n  model: haiku\n", true, "haiku"},
	} {
		tmpFile := t.TempDir() + "/config.yaml"
		os.WriteFile(tmpFile, []byte(tc.body), 0600)
		cfg, err := Load(tmpFile)
		if err != nil {
			t.Fatalf("%q: Load() error: %v", tc.body, err)
		}
		if cfg.ImageOrientEnabled() != tc.enabled || cfg.ImageOrient.Model != tc.model {
			t.Errorf("%q: enabled=%v model=%q, want %v %q", tc.body, cfg.ImageOrientEnabled(), cfg.ImageOrient.Model, tc.enabled, tc.model)
		}
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
