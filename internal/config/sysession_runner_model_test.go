package config

import (
	"os"
	"strings"
	"testing"
)

// A well-formed sysession.runner.model is preserved through Load/validate.
func TestSysessionRunnerModelOverride(t *testing.T) {
	tmpFile := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(tmpFile, []byte("sysession:\n  runner:\n    model: haiku\n"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Sysession.Runner.Model != "haiku" {
		t.Errorf("model = %q, want haiku", cfg.Sysession.Runner.Model)
	}
}

// sysession.runner.model is appended verbatim to the Runner exec argv as
// `--model <value>` (sysession/runner.go). A flag-shaped identifier must be
// rejected by validateConfig so it cannot smuggle extra argv flags
// (R20260614-SEC-1).
func TestSysessionRunnerModelRejectsInjection(t *testing.T) {
	tmpFile := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(tmpFile, []byte("sysession:\n  runner:\n    model: \"--system-prompt evil\"\n"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Load(tmpFile)
	if err == nil {
		t.Fatal("Load() should reject a sysession.runner.model with a space/flag")
	}
	if !strings.Contains(err.Error(), "sysession.runner.model") {
		t.Errorf("error should name sysession.runner.model, got: %v", err)
	}
}
